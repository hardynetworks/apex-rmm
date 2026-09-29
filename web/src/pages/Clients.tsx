import { useState } from 'preact/hooks';
import { api } from '../api';
import { Icon } from '../icons';
import { Link } from '../router';
import { Badge, Button, Card, CopyText, Empty, Field, Modal, PageHeader, Spinner, Tabs, attempt, confirmDialog, fmtDate, timeAgo, useFetch } from '../ui';
import { useCan } from '../user';

export function Clients() {
  const { data, reload } = useFetch<any[]>('/clients');
  const [edit, setEdit] = useState<any | null>(null);
  const [deploy, setDeploy] = useState<string | null>(null);
  const admin = useCan('admin');
  return (
    <div class="page">
      <PageHeader title="Clients" subtitle="Organisations and sites you manage."
        actions={admin && <><Button icon="download" onClick={() => setDeploy('')}>Deploy agent</Button><Button variant="primary" icon="plus" onClick={() => setEdit({})}>New client</Button></>} />
      {!data ? <Spinner /> : !data.length ? <Card><Empty icon="building" title="No clients yet" /></Card> : (
        <div class="client-grid">
          {data.map((c) => (
            <Card key={c.id} title={<span class="title-row"><span class="avatar sm">{c.name.slice(0, 1).toUpperCase()}</span>{c.name}</span>}
              actions={admin && <Button size="sm" variant="ghost" icon="edit" onClick={() => setEdit(c)} title="Edit" />}>
              <div class="client-stats">
                <Link href={'/devices?client=' + c.id}><strong>{c.device_count}</strong><span>devices</span></Link>
                <Link href={'/devices?client=' + c.id + '&status=online'}><strong>{c.online_count}</strong><span>online</span></Link>
                <Link href={'/tickets?client=' + c.id}><strong>{c.open_tickets}</strong><span>open tickets</span></Link>
              </div>
              {(c.contact_name || c.contact_email || c.contact_phone) && (
                <div class="muted small contact"><Icon name="user" size={13} /> {[c.contact_name, c.contact_email, c.contact_phone].filter(Boolean).join(' · ')}</div>
              )}
              <div class="sites">
                {c.sites.map((s: any) => <Badge key={s.id} tone="neutral">{s.name}</Badge>)}
              </div>
              {admin && <div class="card-foot"><Button size="sm" icon="download" onClick={() => setDeploy(c.id)}>Add devices</Button></div>}
            </Card>
          ))}
        </div>
      )}
      {admin && <Tokens />}
      {edit && <ClientEditor c={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload(true); }} />}
      {deploy !== null && <DeployModal clients={data || []} clientId={deploy} onClose={() => setDeploy(null)} />}
    </div>
  );
}

function ClientEditor({ c, onClose, onSaved }: { c: any; onClose: () => void; onSaved: () => void }) {
  const [v, setV] = useState({ name: c.name || '', notes: c.notes || '', contact_name: c.contact_name || '', contact_email: c.contact_email || '', contact_phone: c.contact_phone || '' });
  const [site, setSite] = useState('');
  const [sites, setSites] = useState<any[]>(c.sites || []);
  const set = (k: string, val: string) => setV({ ...v, [k]: val });
  const save = async () => { if (await attempt(() => (c.id ? api.put('/clients/' + c.id, v) : api.post('/clients', v)), 'Client saved')) onSaved(); };
  const del = async () => {
    if (!(await confirmDialog({ title: `Delete ${c.name}?`, body: 'Only clients without devices can be deleted.', danger: true, confirm: 'Delete' }))) return;
    if (await attempt(() => api.del('/clients/' + c.id), 'Client deleted')) onSaved();
  };
  const addSite = async () => {
    if (!site.trim()) return;
    await attempt(async () => { const s = await api.post(`/clients/${c.id}/sites`, { name: site }); setSites([...sites, s]); setSite(''); });
  };
  const delSite = async (s: any) => { if (await attempt(() => api.del('/sites/' + s.id))) setSites(sites.filter((x) => x.id !== s.id)); };
  return (
    <Modal title={c.id ? 'Edit client' : 'New client'} onClose={onClose}
      footer={<>{c.id && <Button variant="danger" icon="trash" onClick={del}>Delete</Button>}<div class="grow" /><Button onClick={onClose}>Cancel</Button><Button variant="primary" onClick={save}>Save</Button></>}>
      <Field label="Name"><input value={v.name} autoFocus onInput={(e) => set('name', (e.target as HTMLInputElement).value)} /></Field>
      <div class="row-fields">
        <Field label="Contact name"><input value={v.contact_name} onInput={(e) => set('contact_name', (e.target as HTMLInputElement).value)} /></Field>
        <Field label="Email"><input value={v.contact_email} onInput={(e) => set('contact_email', (e.target as HTMLInputElement).value)} /></Field>
        <Field label="Phone"><input value={v.contact_phone} onInput={(e) => set('contact_phone', (e.target as HTMLInputElement).value)} /></Field>
      </div>
      <Field label="Notes"><textarea rows={3} value={v.notes} onInput={(e) => set('notes', (e.target as HTMLTextAreaElement).value)} /></Field>
      {c.id && (
        <Field label="Sites">
          <div class="sites">{sites.map((s) => <span key={s.id} class="badge badge-neutral">{s.name} <button class="x" onClick={() => delSite(s)} aria-label={'Remove ' + s.name}>×</button></span>)}</div>
          <div class="inline"><input placeholder="New site name" value={site} onInput={(e) => setSite((e.target as HTMLInputElement).value)} /><Button onClick={addSite}>Add site</Button></div>
        </Field>
      )}
    </Modal>
  );
}

export function DeployModal({ clients, clientId, onClose }: { clients: any[]; clientId?: string; onClose: () => void }) {
  const [client, setClient] = useState(clientId || clients[0]?.id || '');
  const [site, setSite] = useState('');
  const [expires, setExpires] = useState(168);
  const [maxUses, setMaxUses] = useState(0);
  const [token, setToken] = useState<any | null>(null);
  const [os, setOs] = useState('windows');
  const sites = clients.find((c) => c.id === client)?.sites || [];
  const create = () => attempt(async () => setToken(await api.post('/enrollment-tokens', { client_id: client, site_id: site, expires_hours: expires, max_uses: maxUses, description: 'Deploy dialog' })));
  const origin = location.origin;
  return (
    <Modal title="Deploy the agent" onClose={onClose} width={760} footer={<Button onClick={onClose}>Done</Button>}>
      {!token ? (
        <>
          <p class="muted">Create an install link for a client. Anyone with the link can enrol a device into that client until it expires.</p>
          <div class="row-fields">
            <Field label="Client"><select value={client} onChange={(e) => { setClient((e.target as HTMLSelectElement).value); setSite(''); }}>{clients.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
            <Field label="Site"><select value={site} onChange={(e) => setSite((e.target as HTMLSelectElement).value)}><option value="">—</option>{sites.map((s: any) => <option key={s.id} value={s.id}>{s.name}</option>)}</select></Field>
          </div>
          <div class="row-fields">
            <Field label="Expires"><select value={expires} onChange={(e) => setExpires(+(e.target as HTMLSelectElement).value)}><option value={1}>1 hour</option><option value={24}>1 day</option><option value={168}>7 days</option><option value={720}>30 days</option><option value={0}>Never</option></select></Field>
            <Field label="Max installs" hint="0 = unlimited"><input type="number" min={0} value={maxUses} onInput={(e) => setMaxUses(+(e.target as HTMLInputElement).value)} /></Field>
          </div>
          <Button variant="primary" icon="link" disabled={!client} onClick={create}>Create install commands</Button>
        </>
      ) : (
        <>
          <Tabs tabs={[{ id: 'windows', label: 'Windows' }, { id: 'macos', label: 'macOS' }, { id: 'linux', label: 'Linux' }]} active={os} onChange={setOs} />
          {os === 'windows' && <>
            <p>Run in an <b>elevated PowerShell</b> (Run as Administrator):</p>
            <CopyText text={token.urls.windows} />
            <p class="muted small">Works on Windows 10/11 and Server 2016+. Installs to <code>C:\Program Files\HardyRMM</code> as the "Hardy RMM Agent" service.</p>
          </>}
          {os === 'macos' && <>
            <p>Run in Terminal:</p>
            <CopyText text={token.urls.macos} />
            <p class="muted small">Then grant <b>Screen Recording</b> and <b>Accessibility</b> to <code>/usr/local/hardy-agent/hardy-agent</code> in System Settings → Privacy &amp; Security for remote control (or deploy a PPPC profile via MDM).</p>
          </>}
          {os === 'linux' && <>
            <p>Run as root:</p>
            <CopyText text={token.urls.linux} />
            <p class="muted small">Supports systemd, SysV and OpenRC hosts on amd64, arm64 and armv7. Built-in remote control needs an Xorg session; use RustDesk for Wayland.</p>
          </>}
          <details class="mt">
            <summary>Manual install</summary>
            <p class="small">Download the agent (<a class="link" href={`${origin}/download/agent/windows/amd64`}>Windows x64</a> · <a class="link" href={`${origin}/download/agent/darwin/arm64`}>macOS Apple silicon</a> · <a class="link" href={`${origin}/download/agent/darwin/amd64`}>macOS Intel</a> · <a class="link" href={`${origin}/download/agent/linux/amd64`}>Linux x64</a> · <a class="link" href={`${origin}/download/agent/linux/arm64`}>Linux ARM64</a>) and run:</p>
            <CopyText text={`hardy-agent install --server ${origin} --token ${token.token}`} />
          </details>
          <p class="muted small mt">{token.expires_at ? `Expires ${fmtDate(token.expires_at)}` : 'Never expires'}{token.max_uses ? ` · ${token.max_uses} installs max` : ''}. Revoke it any time from the Clients page.</p>
        </>
      )}
    </Modal>
  );
}

function Tokens() {
  const { data, reload } = useFetch<any[]>('/enrollment-tokens');
  if (!data || !data.length) return null;
  const revoke = async (t: any) => {
    if (!(await confirmDialog({ title: 'Revoke this install link?', body: 'Devices already enrolled are not affected.', danger: true, confirm: 'Revoke' }))) return;
    if (await attempt(() => api.del('/enrollment-tokens/' + t.id), 'Install link revoked')) reload(true);
  };
  return (
    <Card title="Install links" pad={false} class="mt">
      <table class="table compact">
        <thead><tr><th>Client</th><th>Created</th><th>Uses</th><th>Expires</th><th>Status</th><th /></tr></thead>
        <tbody>
          {data.map((t) => (
            <tr key={t.id}>
              <td>{t.client_name}{t.site_name && <span class="muted"> / {t.site_name}</span>}<div class="muted small mono">{t.token.slice(0, 8)}…</div></td>
              <td class="muted">{timeAgo(t.created_at)} by {t.created_by}</td>
              <td>{t.uses}{t.max_uses ? ' / ' + t.max_uses : ''}</td>
              <td class="muted">{t.expires_at ? fmtDate(t.expires_at) : 'never'}</td>
              <td>{t.active ? <Badge tone="good">active</Badge> : <Badge>{t.revoked ? 'revoked' : 'expired'}</Badge>}</td>
              <td class="right nowrap">{t.active && <><Button size="sm" variant="ghost" icon="copy" onClick={() => navigator.clipboard.writeText(t.urls.windows)} title="Copy Windows command" /><Button size="sm" variant="ghost" onClick={() => revoke(t)}>Revoke</Button></>}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Card>
  );
}
