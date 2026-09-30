import { useEffect, useState } from 'preact/hooks';
import { api } from '../api';
import { navigate, useQuery } from '../router';
import { Badge, Button, Card, CopyText, Empty, Field, Kv, PageHeader, SearchInput, Spinner, Tabs, attempt, fmtDate, timeAgo, toast, useFetch } from '../ui';
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
  return (
    <div class="stack">
      <SystemSettings />
      <div class="grid-2">
        <Notifications />
        <Account />
      </div>
    </div>
  );
}

const SYS_FIELDS = ['public_url', 'company_name', 'oidc_issuer', 'oidc_client_id', 'oidc_admin_groups', 'oidc_tech_groups', 'oidc_viewer_groups', 'oidc_default_role', 'oidc_logout_url', 'rustdesk_host', 'rustdesk_relay', 'rustdesk_key'];

function SystemSettings() {
  const { data, setData } = useFetch<any>('/settings/system');
  const [v, setV] = useState<Record<string, string>>({});
  const [secret, setSecret] = useState('');
  const [busy, setBusy] = useState(false);
  const [test, setTest] = useState<any>(null);
  const [testing, setTesting] = useState(false);
  useEffect(() => { if (data) setV({ ...data.values }); }, [data]);
  if (!data) return <Spinner />;
  const locked = (k: string) => !!data.locked?.[k];
  const set = (k: string, val: string) => setV({ ...v, [k]: val });
  const input = (k: string, props: any = {}) => (
    <input value={v[k] ?? ''} disabled={locked(k)} onInput={(e) => set(k, (e.target as HTMLInputElement).value)} {...props} />
  );
  const hint = (k: string, text?: any) => (locked(k) ? <span class="text-warn">Set by an environment variable in .env — remove it there to edit here.</span> : text);
  const dirty = data && (SYS_FIELDS.some((k) => (v[k] ?? '') !== (data.values[k] ?? '')) || secret !== '');
  const save = async () => {
    setBusy(true);
    try {
      const body: any = {};
      SYS_FIELDS.forEach((k) => (body[k] = v[k] ?? ''));
      if (secret) body.oidc_client_secret = secret;
      const res = await api.put('/settings/system', body);
      setData(res);
      setSecret('');
      if (res.sso_error) toast('Saved, but single sign-on could not start: ' + res.sso_error, 'error');
      else toast(res.sso_active ? 'Saved — single sign-on is active' : 'Settings saved');
    } catch (e: any) {
      toast(e.message, 'error');
    }
    setBusy(false);
  };
  const runTest = async () => {
    setTesting(true);
    setTest(null);
    try { setTest(await api.post('/settings/test-oidc', { issuer: v.oidc_issuer })); } catch (e: any) { setTest({ ok: false, error: e.message }); }
    setTesting(false);
  };
  const origin = (v.public_url || location.origin).replace(/\/$/, '');
  return (
    <>
      <div class="grid-2">
        <Card title="Server">
          <Field label="Public URL" hint={hint('public_url', 'The address people and agents use to reach this server, e.g. https://rmm.example.com. Existing agents keep the address they were installed with.')}>
            {input('public_url', { placeholder: location.origin })}
          </Field>
          {!locked('public_url') && v.public_url !== location.origin && (
            <button class="chip" onClick={() => set('public_url', location.origin)}>Use {location.origin}</button>
          )}
          <Field label="Company name" hint={hint('company_name', 'Shown in the sidebar and on the sign-in page.')}>{input('company_name')}</Field>
        </Card>
        <Card title="RustDesk server" actions={<span class="muted small">backup remote access</span>}>
          <Field label="ID / relay server host" hint={hint('rustdesk_host', 'Hostname or IP devices use to reach hbbs on ports 21115-21119 (DNS-only if you use Cloudflare).')}>
            {input('rustdesk_host', { placeholder: 'rustdesk.example.com' })}
          </Field>
          <Field label="Relay server (optional)" hint={hint('rustdesk_relay', 'Leave empty to use the same host.')}>{input('rustdesk_relay')}</Field>
          <Field label="Public key (optional)" hint={hint('rustdesk_key', data.rustdesk_key_detected ? 'Leave empty to use the key detected from the bundled RustDesk server.' : 'The bundled RustDesk server key was not found; paste id_ed25519.pub here.')}>
            {input('rustdesk_key', { placeholder: data.rustdesk_key_detected || '' })}
          </Field>
          {data.rustdesk_key_detected && <div class="small muted">Detected key: <CopyText text={data.rustdesk_key_detected} /></div>}
        </Card>
      </div>

      <Card title={<span class="title-row">Single sign-on (Authentik / OIDC) {data.sso_active ? <Badge tone="good">active</Badge> : v.oidc_issuer ? <Badge tone="warning">not active</Badge> : <Badge>off</Badge>}</span>}>
        {data.sso_error && <div class="errorbox">{data.sso_error}</div>}
        <div class="form-grid">
          <div>
            <Field label="Redirect URI" hint="Paste this into the Authentik provider (Redirect URIs, strict).">
              <CopyText text={origin + '/auth/callback'} />
            </Field>
            <Field label="Issuer URL" hint={hint('oidc_issuer', `In Authentik: the provider's "OpenID Configuration Issuer", e.g. https://auth.example.com/application/o/apex-rmm/`)}>
              <div class="inline" style="margin-top:0">
                {input('oidc_issuer', { placeholder: 'https://auth.example.com/application/o/apex-rmm/' })}
                <Button onClick={runTest} loading={testing} disabled={!v.oidc_issuer}>Test</Button>
              </div>
            </Field>
            {test && (test.ok
              ? <div class="small text-good mb">Issuer reachable ✓ {test.algs?.length ? `(signing: ${test.algs.join(', ')})` : ''}{test.algs && !test.algs.some((a: string) => a.startsWith('RS') || a.startsWith('ES')) && <span class="text-warn"> — pick a signing key in Authentik (RS256)</span>}</div>
              : <div class="errorbox small">{test.error}</div>)}
            <Field label="Client ID" hint={hint('oidc_client_id')}>{input('oidc_client_id')}</Field>
            <Field label="Client secret" hint={locked('oidc_client_secret') ? hint('oidc_client_secret') : data.oidc_client_secret_set ? 'A secret is saved. Leave empty to keep it.' : 'Stored encrypted.'}>
              <input type="password" autoComplete="new-password" value={secret} disabled={locked('oidc_client_secret')} placeholder={data.oidc_client_secret_set ? '••••••••••••' : ''} onInput={(e) => setSecret((e.target as HTMLInputElement).value)} />
            </Field>
          </div>
          <div>
            <Field label="Admin groups" hint={hint('oidc_admin_groups', 'Comma separated Authentik group names.')}>{input('oidc_admin_groups')}</Field>
            <Field label="Technician groups" hint={hint('oidc_tech_groups')}>{input('oidc_tech_groups')}</Field>
            <Field label="Viewer groups (optional)" hint={hint('oidc_viewer_groups')}>{input('oidc_viewer_groups')}</Field>
            <Field label="Users in no group" hint={hint('oidc_default_role')}>
              <select value={v.oidc_default_role ?? ''} disabled={locked('oidc_default_role')} onChange={(e) => set('oidc_default_role', (e.target as HTMLSelectElement).value)}>
                <option value="">Deny access</option><option value="viewer">Viewer</option><option value="technician">Technician</option><option value="admin">Admin</option>
              </select>
            </Field>
          </div>
        </div>
        <p class="muted small">Keep your local admin login until you've confirmed SSO works in a private window. Step-by-step Authentik instructions are in docs/AUTHENTIK.md.</p>
      </Card>
      <div class="form-actions sticky-save">
        {dirty && <span class="muted small">Unsaved changes</span>}
        <Button onClick={() => { setV({ ...data.values }); setSecret(''); }} disabled={!dirty}>Discard</Button>
        <Button variant="primary" loading={busy} disabled={!dirty} onClick={save}>Save settings</Button>
      </div>
    </>
  );
}

function Notifications() {
  const { data } = useFetch<any>('/settings');
  const [s, setS] = useState<any>(null);
  useEffect(() => { if (data) setS(data.settings); }, [data]);
  if (!data || !s) return <Spinner />;
  const save = () => attempt(() => api.put('/settings', s), 'Settings saved');
  const test = () => attempt(async () => { await api.put('/settings', s); await api.post('/settings/test-webhook'); }, 'Test notification sent');
  return (
    <Card title="Alert notifications">
      <Field label="Webhook URL" hint="Discord, Slack, ntfy or any endpoint accepting JSON."><input value={s.webhook_url} placeholder="https://…" onInput={(e) => setS({ ...s, webhook_url: (e.target as HTMLInputElement).value })} /></Field>
      <Field label="Format">
        <select value={s.webhook_format} onChange={(e) => setS({ ...s, webhook_format: (e.target as HTMLSelectElement).value })}>
          <option value="generic">Generic JSON</option><option value="discord">Discord</option><option value="slack">Slack</option><option value="ntfy">ntfy</option>
        </select>
      </Field>
      <div class="form-actions"><Button onClick={test} disabled={!s.webhook_url}>Send test</Button><Button variant="primary" onClick={save}>Save</Button></div>
    </Card>
  );
}

function Account() {
  const user = useUser();
  const [cur, setCur] = useState('');
  const [pw, setPw] = useState('');
  const [pw2, setPw2] = useState('');
  if (!user.local) return (
    <Card title="Your account">
      <Kv items={[['Signed in as', user.email], ['Role', user.role], ['Sign-in method', 'Single sign-on']]} />
      <p class="muted small">Your password is managed in Authentik.</p>
    </Card>
  );
  const change = async () => {
    if (pw !== pw2) return toast('The new passwords do not match', 'error');
    if (await attempt(() => api.post('/auth/password', { current: cur, new: pw }), 'Password changed')) { setCur(''); setPw(''); setPw2(''); }
  };
  return (
    <Card title="Your account (local)">
      <Field label="Current password"><input type="password" autoComplete="current-password" value={cur} onInput={(e) => setCur((e.target as HTMLInputElement).value)} /></Field>
      <Field label="New password" hint="At least 10 characters"><input type="password" autoComplete="new-password" value={pw} onInput={(e) => setPw((e.target as HTMLInputElement).value)} /></Field>
      <Field label="Repeat new password"><input type="password" autoComplete="new-password" value={pw2} onInput={(e) => setPw2((e.target as HTMLInputElement).value)} /></Field>
      <div class="form-actions"><Button variant="primary" disabled={!cur || pw.length < 10} onClick={change}>Change password</Button></div>
    </Card>
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
