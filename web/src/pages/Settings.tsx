import { useEffect, useState } from 'preact/hooks';
import { api } from '../api';
import { navigate, useQuery } from '../router';
import { Badge, Button, Card, CopyText, Empty, Field, Kv, PageHeader, SearchInput, Spinner, Tabs, attempt, fmtDate, timeAgo, useFetch } from '../ui';
import { useCan, useUser } from '../user';

export function Settings() {
  const q = useQuery();
  const admin = useCan('admin');
  const tab = q.get('tab') || (admin ? 'general' : 'users');
  const tabs = admin
    ? [{ id: 'general', label: 'General', icon: 'settings' }, { id: 'users', label: 'Users & roles', icon: 'users' }, { id: 'audit', label: 'Audit log', icon: 'history' }]
    : [{ id: 'users', label: 'Users & roles', icon: 'users' }];
  return (
    <div class="page">
      <PageHeader title="Settings" />
      <Tabs tabs={tabs} active={tab} onChange={(t) => navigate('/settings?tab=' + t, true)} />
      {tab === 'general' && admin && <General />}
      {tab === 'users' && <Users />}
      {tab === 'audit' && admin && <Audit />}
    </div>
  );
}

function General() {
  const { data } = useFetch<any>('/settings');
  const [s, setS] = useState<any>(null);
  useEffect(() => { if (data) setS(data.settings); }, [data]);
  if (!data || !s) return <Spinner />;
  const srv = data.server;
  const save = () => attempt(() => api.put('/settings', s), 'Settings saved');
  const test = () => attempt(async () => { await api.put('/settings', s); await api.post('/settings/test-webhook'); }, 'Test notification sent');
  return (
    <div class="grid-2">
      <Card title="Alert notifications">
        <Field label="Webhook URL" hint="Discord, Slack, ntfy or any endpoint accepting JSON."><input value={s.webhook_url} placeholder="https://…" onInput={(e) => setS({ ...s, webhook_url: (e.target as HTMLInputElement).value })} /></Field>
        <Field label="Format">
          <select value={s.webhook_format} onChange={(e) => setS({ ...s, webhook_format: (e.target as HTMLSelectElement).value })}>
            <option value="generic">Generic JSON</option><option value="discord">Discord</option><option value="slack">Slack</option><option value="ntfy">ntfy</option>
          </select>
        </Field>
        <div class="form-actions"><Button onClick={test} disabled={!s.webhook_url}>Send test</Button><Button variant="primary" onClick={save}>Save</Button></div>
      </Card>
      <Card title="Single sign-on (Authentik)">
        <Kv items={[
          ['Status', srv.sso_enabled ? <Badge tone="good">enabled</Badge> : <Badge tone="warning">not configured</Badge>],
          ['Issuer', srv.oidc_issuer || '—'],
          ['Redirect URI', <CopyText text={srv.public_url + '/auth/callback'} />],
          ['Admin groups', (srv.admin_groups || []).join(', ')],
          ['Technician groups', (srv.tech_groups || []).join(', ')],
          ['Viewer groups', (srv.viewer_groups || []).join(', ') || '—'],
          ['Default role', srv.default_role || 'deny access'],
          ['Local break-glass admin', srv.local_admin ? 'enabled' : 'disabled'],
        ]} />
        <p class="muted small">SSO is configured with environment variables (<code>OIDC_*</code>) in <code>.env</code>. Roles are re-synced from Authentik group membership at every sign-in.</p>
      </Card>
      <Card title="RustDesk server">
        <Kv items={[
          ['ID / relay server', srv.rustdesk_host || <Badge tone="warning">RUSTDESK_HOST not set</Badge>],
          ['Public key', srv.rustdesk_key ? <CopyText text={srv.rustdesk_key} /> : <span class="muted">not found</span>],
        ]} />
        <p class="muted small">Enter these in your own RustDesk client (Settings → Network → ID/Relay server) to connect to devices provisioned from their device page.</p>
      </Card>
      <Card title="Server">
        <Kv items={[['Public URL', srv.public_url], ['Metrics retention', srv.metrics_days + ' days']]} />
      </Card>
    </div>
  );
}

function Users() {
  const { data, reload } = useFetch<any[]>('/users');
  const admin = useCan('admin');
  const me = useUser();
  const toggle = async (u: any) => { if (await attempt(() => api.patch('/users/' + u.id, { disabled: !u.disabled }), u.disabled ? 'User enabled' : 'User disabled')) reload(true); };
  if (!data) return <Spinner />;
  return (
    <Card pad={false} title="Users" actions={<span class="muted small">Users are created on first sign-in. Roles come from Authentik groups.</span>}>
      <table class="table">
        <thead><tr><th>User</th><th>Role</th><th class="hide-sm">Groups</th><th>Last sign-in</th><th /></tr></thead>
        <tbody>
          {data.map((u) => (
            <tr key={u.id}>
              <td><span class="strong">{u.name || u.username}</span>{u.id === me.id && <Badge tone="accent">you</Badge>}<div class="muted small">{u.email}{u.local && ' · local account'}</div></td>
              <td><Badge tone={u.role === 'admin' ? 'accent' : u.role === 'technician' ? 'info' : 'neutral'}>{u.role}</Badge></td>
              <td class="hide-sm small muted">{(u.groups || []).join(', ')}</td>
              <td class="muted">{timeAgo(u.last_login)}</td>
              <td class="right">{admin && u.id !== me.id && <Button size="sm" variant="ghost" onClick={() => toggle(u)}>{u.disabled ? 'Enable' : 'Disable'}</Button>}{u.disabled && <Badge tone="critical">disabled</Badge>}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Card>
  );
}

function Audit() {
  const [q, setQ] = useState('');
  const [debounced, setDebounced] = useState('');
  useEffect(() => { const t = setTimeout(() => setDebounced(q), 300); return () => clearTimeout(t); }, [q]);
  const { data } = useFetch<any[]>('/audit?limit=300' + (debounced ? '&q=' + encodeURIComponent(debounced) : ''));
  return (
    <>
      <div class="toolbar"><SearchInput value={q} onInput={setQ} placeholder="Filter by action, user, target…" /></div>
      <Card pad={false}>
        {!data ? <Spinner /> : !data.length ? <Empty icon="history" title="No audit events" /> : (
          <div class="table-wrap">
            <table class="table compact">
              <thead><tr><th>When</th><th>User</th><th>Action</th><th class="hide-sm">Target</th><th class="hide-md">Details</th><th class="hide-sm">IP</th></tr></thead>
              <tbody>
                {data.map((a) => (
                  <tr key={a.id}>
                    <td class="nowrap" title={fmtDate(a.created_at)}>{timeAgo(a.created_at)}</td>
                    <td>{a.user_name}</td>
                    <td class="mono small">{a.action}</td>
                    <td class="hide-sm small">{a.target_type} <span class="muted mono">{String(a.target_id).slice(0, 8)}</span></td>
                    <td class="hide-md small muted truncate">{Object.keys(a.details || {}).length ? JSON.stringify(a.details) : ''}</td>
                    <td class="hide-sm mono small">{a.ip}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  );
}
