import { useEffect, useState } from 'preact/hooks';
import { api } from '../api';
import { Icon } from '../icons';
import { Link } from '../router';
import { Badge, Button, Card, Empty, Field, Spinner, attempt, confirmDialog, useFetch } from '../ui';
import { useCan } from '../user';
import { fmtW } from './Power';

// Smart plugs: Settings → Smart plugs (TP-Link Tapo account and plugs, the Matter server and
// its plugs, assignment, switching) and the plug card on a device's Power tab.

const STATE: Record<string, [string, string]> = {
  connected: ['good', 'Connected'],
  connecting: ['warning', 'Connecting…'],
  error: ['critical', 'Not reachable'],
  off: ['neutral', 'Off'],
};

/** One plug of either kind, with the API path that addresses it. */
type AnyPlug = { kind: 'matter' | 'tapo'; key: string; path: string; [k: string]: any };

function withKind(matter: any, tapo: any): AnyPlug[] {
  return [
    ...(tapo?.plugs || []).map((p: any) => ({ ...p, kind: 'tapo', key: 't' + p.id, path: `/tapo/plugs/${p.id}` })),
    ...(matter?.plugs || []).map((p: any) => ({ ...p, kind: 'matter', key: 'm' + p.node_id, path: `/matter/plugs/${p.node_id}` })),
  ];
}

function plugTarget(p: any): string {
  if (p.device_id) return p.device_name || 'a device';
  if (p.vs_conn) return `${p.host_name || p.host_ref} (${p.conn_name || 'vSphere'})`;
  return '';
}

/** Turn on / off / power cycle, with a confirmation that names what loses power. */
export async function plugPower(p: AnyPlug, action: 'on' | 'off' | 'cycle', done: () => void) {
  const what = plugTarget(p);
  if (action !== 'on') {
    const ok = await confirmDialog({
      title: action === 'cycle' ? `Power cycle ${p.name}?` : `Turn off ${p.name}?`,
      body: (what ? `${what} loses power${action === 'cycle' ? ' for 10 seconds' : ''}: anything running on it stops without shutting down. ` : '')
        + (action === 'cycle' ? 'The plug switches back on by itself after 10 seconds.' : 'It stays off until you turn it back on.'),
      confirm: action === 'cycle' ? 'Power cycle' : 'Turn off',
      danger: true,
    });
    if (!ok) return;
  }
  if (await attempt(() => api.post(`${p.path}/power`, { action, seconds: 10 }),
    action === 'cycle' ? `${p.name}: off, back on in 10 seconds` : `${p.name} turned ${action}`)) {
    setTimeout(done, 800);
  }
}

function PlugState({ plug }: { plug: any }) {
  if (!plug) return <Badge tone="neutral">Not on the Matter server</Badge>;
  if (!plug.available) return <span title={plug.error || ''}><Badge tone="critical">{plug.error === 'not read yet' ? 'Connecting…' : 'Offline'}</Badge></span>;
  if (plug.on == null) return <Badge tone="neutral">No switch</Badge>;
  return plug.on ? <Badge tone="good">On</Badge> : <Badge tone="warning">Off</Badge>;
}

function PlugPower({ plug }: { plug: any }) {
  if (!plug) return <span class="muted">—</span>;
  if (plug.watts == null) {
    return plug.available
      ? <span class="muted small" title="This plug doesn't measure power (e.g. Linkind / AiDot plugs, Tapo P100/P105)">no power meter</span>
      : <span class="muted">—</span>;
  }
  const sub = [plug.volts != null ? Math.round(plug.volts) + ' V' : '', plug.amps != null ? plug.amps.toFixed(2) + ' A' : '',
    plug.today_kwh != null ? plug.today_kwh.toFixed(2) + ' kWh today' : ''].filter(Boolean).join(' · ');
  return (
    <span>
      <strong>{fmtW(plug.watts)}</strong>
      {sub && <div class="muted small">{sub}</div>}
    </span>
  );
}

function plugSubtitle(p: AnyPlug): string {
  const pl = p.plug;
  const model = pl ? [pl.vendor, pl.product].filter(Boolean).join(' ') : '';
  if (p.kind === 'tapo') return [model || 'TP-Link Tapo', p.host].join(' · ');
  return [model || 'node ' + p.node_id, 'Matter'].join(' · ');
}

/** Settings → Smart plugs. */
export function SmartPlugsSettings() {
  const matter = useFetch<any>('/matter', [], 5000);
  const tapo = useFetch<any>('/tapo', [], 5000);
  const devices = useFetch<any[]>('/devices');
  const conns = useFetch<any>('/vsphere/connections');
  const [hosts, setHosts] = useState<{ conn: string; connName: string; ref: string; name: string }[]>([]);
  const [editing, setEditing] = useState<{ key: string; name: string } | null>(null);

  // every vSphere host, for the "powers" choice
  const connList: any[] = Array.isArray(conns.data) ? conns.data : conns.data?.connections || [];
  useEffect(() => {
    let stop = false;
    Promise.all(connList.map((c) => api.get<any>(`/vsphere/connections/${c.id}/inventory`).then((r) =>
      (r.inventory?.hosts || []).map((h: any) => ({ conn: c.id, connName: c.name, ref: h.ref, name: h.name }))).catch(() => [])))
      .then((all) => { if (!stop) setHosts(all.flat()); });
    return () => { stop = true; };
  }, [connList.map((c) => c.id).join()]);

  if (!matter.data || !tapo.data) return <Spinner />;
  const reload = () => { matter.reload(true); tapo.reload(true); };
  const plugs = withKind(matter.data, tapo.data);

  const assign = async (p: AnyPlug, v: string) => {
    const body = v === '' ? { device_id: '' } : v.startsWith('d:') ? { device_id: v.slice(2) } : { vs_conn: v.slice(2).split('|')[0], host_ref: v.slice(2).split('|')[1] };
    if (await attempt(() => api.patch(p.path, body), v ? 'Plug assigned' : 'Plug unassigned')) reload();
  };
  const rename = async () => {
    if (!editing) return;
    const e = editing;
    setEditing(null);
    const p = plugs.find((x) => x.key === e.key);
    if (!p || !e.name.trim() || e.name.trim() === p.name) return;
    if (await attempt(() => api.patch(p.path, { name: e.name.trim() }), 'Renamed')) reload();
  };
  const remove = async (p: AnyPlug) => {
    const body = p.kind === 'matter'
      ? 'The plug is removed from Apex and from the Matter server. It keeps working in other apps it is paired with.'
      : 'Apex stops reading this plug. Nothing changes on the plug or in the Tapo app.';
    if (!(await confirmDialog({ title: `Remove ${p.name}?`, body, confirm: 'Remove', danger: true }))) return;
    if (await attempt(() => api.del(p.path), 'Plug removed')) reload();
  };

  return (
    <div class="stack">
      <TapoCard data={tapo.data} reload={() => tapo.reload(true)} />
      <MatterCards data={matter.data} reload={() => matter.reload(true)} />

      <Card title="Smart plugs" icon="bolt" pad={false}>
        {!plugs.length ? <Empty icon="bolt" title="No plugs yet">Add a Tapo plug or pair a Matter plug above.</Empty> : (
          <div class="table-wrap">
            <table class="table">
              <thead><tr><th>Plug</th><th>State</th><th>Power</th><th>Powers</th><th /></tr></thead>
              <tbody>
                {plugs.map((p) => {
                  const pl = p.plug;
                  const value = p.device_id ? 'd:' + p.device_id : p.vs_conn ? `h:${p.vs_conn}|${p.host_ref}` : '';
                  return (
                    <tr key={p.key}>
                      <td>
                        {editing?.key === p.key
                          ? <input class="inline-edit" value={editing?.name} autoFocus onInput={(e) => setEditing({ key: p.key, name: (e.target as HTMLInputElement).value })}
                              onBlur={rename} onKeyDown={(e) => { if (e.key === 'Enter') rename(); if (e.key === 'Escape') setEditing(null); }} />
                          : <button class="linklike strong" onClick={() => setEditing({ key: p.key, name: p.name })} title="Rename">{p.name} <Icon name="edit" size={12} /></button>}
                        <div class="muted small">{plugSubtitle(p)}</div>
                        {pl && !pl.available && pl.error && pl.error !== 'not read yet' && <div class="small text-warn plug-error">{pl.error}</div>}
                      </td>
                      <td><PlugState plug={pl} /></td>
                      <td class="nowrap"><PlugPower plug={pl} /></td>
                      <td>
                        <select value={value} onChange={(e) => assign(p, (e.target as HTMLSelectElement).value)} aria-label="Powers">
                          <option value="">Not assigned</option>
                          {hosts.length > 0 && <optgroup label="ESXi hosts">{hosts.map((h) => <option key={h.conn + h.ref} value={`h:${h.conn}|${h.ref}`}>{h.name}{connList.length > 1 ? ` (${h.connName})` : ''}</option>)}</optgroup>}
                          <optgroup label="Devices">{(devices.data || []).map((d) => <option key={d.id} value={'d:' + d.id}>{d.display_name || d.hostname}</option>)}</optgroup>
                        </select>
                        {pl && pl.available && pl.watts == null && value && <div class="muted small">State only: this plug doesn't measure power</div>}
                      </td>
                      <td class="nowrap right">
                        {pl?.on === false
                          ? <Button size="sm" icon="power" onClick={() => plugPower(p, 'on', reload)}>Turn on</Button>
                          : <><Button size="sm" icon="refresh" disabled={!pl?.available} onClick={() => plugPower(p, 'cycle', reload)}>Power cycle</Button>
                            <Button size="sm" variant="ghost" disabled={!pl?.available} onClick={() => plugPower(p, 'off', reload)}>Off</Button></>}
                        <Button size="sm" variant="ghost" icon="trash" title="Remove" onClick={() => remove(p)} />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>
      <p class="muted small">A plug that measures power replaces the device's or host's own power reading. Plugs without a power meter, like Linkind's, can still power cycle what they're assigned to.</p>
    </div>
  );
}

/** TP-Link account and adding a Tapo plug by IP address. */
function TapoCard({ data, reload }: { data: any; reload: () => void }) {
  const [user, setUser] = useState(data.username || '');
  const [pass, setPass] = useState('');
  const [busy, setBusy] = useState(false);
  const [host, setHost] = useState('');
  const [name, setName] = useState('');
  const [adding, setAdding] = useState(false);
  useEffect(() => setUser(data.username || ''), [data.username]);

  const save = async () => {
    setBusy(true);
    if (await attempt(() => api.put('/tapo/settings', { username: user, password: pass }), user ? 'TP-Link account saved' : 'TP-Link account removed')) {
      setPass('');
      reload();
    }
    setBusy(false);
  };
  const add = async () => {
    setAdding(true);
    if (await attempt(() => api.post('/tapo/plugs', { host, name }), 'Plug added')) {
      setHost(''); setName(''); reload();
    }
    setAdding(false);
  };
  return (
    <Card title="TP-Link Tapo plugs" icon="bolt">
      <p class="muted small" style={{ marginTop: 0 }}>
        Wi-Fi plugs such as the P110 and P115 are read directly by the Apex server on your network, using the TP-Link account they were set up with.
        In the Tapo app, open <strong>Me → Third-Party Services</strong> and turn on <strong>Third-Party Compatibility</strong> first (newer firmware refuses local logins without it),
        and give each plug a fixed IP (a DHCP reservation on your router).
      </p>
      <div class="form-grid">
        <Field label="TP-Link ID (email)" hint="The account in the Tapo app; case-sensitive">
          <input value={user} autoComplete="off" onInput={(e) => setUser((e.target as HTMLInputElement).value)} placeholder="you@example.com" />
        </Field>
        <Field label="Password" hint={data.configured ? 'Saved. Leave empty to keep it' : 'Stored encrypted; never shown again'}>
          <input type="password" value={pass} autoComplete="new-password" onInput={(e) => setPass((e.target as HTMLInputElement).value)} placeholder={data.configured ? '••••••••' : ''} />
        </Field>
      </div>
      <div class="form-actions">
        <span class="title-row">{data.configured ? <Badge tone="good">Account saved</Badge> : <Badge tone="neutral">No account</Badge>}</span>
        <span class="grow" />
        <Button variant="primary" loading={busy} disabled={!user.trim() && !data.username} onClick={save}>Save account</Button>
      </div>
      <div class="form-grid" style={{ marginTop: 12 }}>
        <Field label="Plug IP address" hint="Tapo app → the plug → ⚙ → Device Info">
          <input value={host} autoComplete="off" onInput={(e) => setHost((e.target as HTMLInputElement).value)} placeholder="10.0.0.50" />
        </Field>
        <Field label="Name" hint="Optional; defaults to the plug's name in the Tapo app">
          <input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
        </Field>
      </div>
      <div class="form-actions">
        <span class="grow" />
        <Button variant="primary" icon="plus" loading={adding} disabled={!data.configured || !host.trim()} onClick={add}>Add Tapo plug</Button>
      </div>
    </Card>
  );
}

/** Matter server connection and pairing. */
function MatterCards({ data, reload }: { data: any; reload: () => void }) {
  const [url, setUrl] = useState('');
  const [enabled, setEnabled] = useState(false);
  const [busy, setBusy] = useState(false);
  const [code, setCode] = useState('');
  const [name, setName] = useState('');
  const [pairing, setPairing] = useState(false);
  useEffect(() => { setUrl(data.url); setEnabled(data.enabled); }, [data.url, data.enabled]);
  const st = data.status || { state: 'off' };
  const [tone, label] = STATE[st.state] || STATE.off;

  const saveSettings = async () => {
    setBusy(true);
    await attempt(async () => { await api.put('/matter/settings', { enabled, url }); reload(); }, enabled ? 'Matter turned on' : 'Matter turned off');
    setBusy(false);
  };
  const pair = async () => {
    setPairing(true);
    const ok = await attempt(() => api.post('/matter/plugs', { code, name }), 'Plug added');
    setPairing(false);
    if (ok) { setCode(''); setName(''); reload(); }
  };
  return (
    <>
      <Card title="Matter server" icon="network">
        <p class="muted small" style={{ marginTop: 0 }}>
          Apex talks to Matter smart plugs through a Matter controller (matterjs-server). Start it next to Apex with{' '}
          <code>docker compose --profile matter up -d</code>, then turn Matter on here.
        </p>
        <div class="form-grid">
          <Field label="Matter" hint="Connect to the Matter server">
            <label class="check"><input type="checkbox" checked={enabled} disabled={data.locked} onChange={(e) => setEnabled((e.target as HTMLInputElement).checked)} /> Enabled</label>
          </Field>
          <Field label="Server address" hint={data.locked ? 'Set by APEX_MATTER_URL' : 'WebSocket URL of matterjs-server'}>
            <input value={url} disabled={data.locked} onInput={(e) => setUrl((e.target as HTMLInputElement).value)} placeholder="ws://host.docker.internal:5580/ws" />
          </Field>
        </div>
        <div class="form-actions">
          <span class="title-row"><Badge tone={tone}>{label}</Badge>{st.error && <span class="muted small">{st.error}</span>}</span>
          <span class="grow" />
          {!data.locked && <Button variant="primary" loading={busy} onClick={saveSettings}>Save</Button>}
        </div>
      </Card>

      {data.enabled && (
        <Card title="Add a Matter plug" icon="plus">
          <p class="muted small" style={{ marginTop: 0 }}>
            Set the plug up in its own app first (for Linkind plugs, the AiDot app; or Apple Home / Google Home), then open the plug's settings there and choose
            <strong> Turn on pairing mode</strong> / <strong>Linked Matter apps → Link apps &amp; services</strong>. Enter the code it shows. The plug keeps working in that app too.
          </p>
          <div class="form-grid">
            <Field label="Pairing code" hint="11 digits (1234-567-8901) or the MT:… text">
              <input value={code} onInput={(e) => setCode((e.target as HTMLInputElement).value)} placeholder="3497-011-2332" autoComplete="off" />
            </Field>
            <Field label="Name" hint="Optional, e.g. “Rack 1 – ESXi 102”">
              <input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
            </Field>
          </div>
          <div class="form-actions">
            {pairing && <span class="muted small">Pairing can take up to two minutes…</span>}
            <span class="grow" />
            <Button variant="primary" icon="plus" loading={pairing} disabled={st.state !== 'connected' || !code.trim()} onClick={pair}>Pair plug</Button>
          </div>
        </Card>
      )}
    </>
  );
}

/** The plug card on a device's Power tab. */
export function DevicePlugCard({ deviceId }: { deviceId: string }) {
  const admin = useCan('admin');
  const matter = useFetch<any>(`/matter?device=${deviceId}`, [deviceId], 10000);
  const tapo = useFetch<any>(`/tapo?device=${deviceId}`, [deviceId], 10000);
  if (!matter.data || !tapo.data) return null;
  const reload = () => { matter.reload(true); tapo.reload(true); };
  const all = withKind(matter.data, tapo.data);
  const p = all.find((x) => x.kind === 'matter') || all[0];
  const anyConfigured = matter.data.enabled || tapo.data.configured;
  if (!p) {
    if (!admin || !anyConfigured) return null;
    return (
      <Card title="Smart plug" icon="bolt">
        <p class="muted small" style={{ margin: 0 }}>No smart plug powers this device. <Link href="/settings?tab=plugs">Assign one in Settings → Smart plugs</Link>.</p>
      </Card>
    );
  }
  const pl = p.plug;
  return (
    <Card title="Smart plug" icon="bolt" actions={admin && <Link href="/settings?tab=plugs" class="link">Manage</Link>}>
      <div class="plug-row">
        <div>
          <div class="strong">{p.name}</div>
          <div class="muted small">{plugSubtitle(p)}</div>
        </div>
        <PlugState plug={pl} />
        <PlugPower plug={pl} />
      </div>
      {pl && !pl.available && pl.error && <p class="small text-warn">{pl.error}</p>}
      {admin && pl && (
        <div class="form-actions">
          {pl.on === false
            ? <Button size="sm" icon="power" onClick={() => plugPower(p, 'on', reload)}>Turn on</Button>
            : <><Button size="sm" icon="refresh" disabled={!pl.available} onClick={() => plugPower(p, 'cycle', reload)}>Power cycle</Button>
              <Button size="sm" variant="ghost" disabled={!pl.available} onClick={() => plugPower(p, 'off', reload)}>Turn off</Button></>}
        </div>
      )}
      {p.kind === 'matter' && matter.data.status?.state !== 'connected' && <p class="muted small">The Matter server isn't connected; the plug's state may be out of date.</p>}
    </Card>
  );
}
