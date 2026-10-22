import { useEffect, useState } from 'preact/hooks';
import { api } from '../api';
import { Icon } from '../icons';
import { Link } from '../router';
import { Badge, Button, Card, Empty, Field, Spinner, attempt, confirmDialog, useFetch } from '../ui';
import { useCan } from '../user';
import { fmtW } from './Power';

// Matter smart plugs: Settings → Smart plugs (connection, pairing, assignment, switching) and the
// plug card on a device's Power tab.

const STATE: Record<string, [string, string]> = {
  connected: ['good', 'Connected'],
  connecting: ['warning', 'Connecting…'],
  error: ['critical', 'Not reachable'],
  off: ['neutral', 'Off'],
};

function plugTarget(p: any): string {
  if (p.device_id) return p.device_name || 'a device';
  if (p.vs_conn) return `${p.host_name || p.host_ref} (${p.conn_name || 'vSphere'})`;
  return '';
}

/** Turn on / off / power cycle, with a confirmation that names what loses power. */
export async function plugPower(p: any, action: 'on' | 'off' | 'cycle', done: () => void) {
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
  if (await attempt(() => api.post(`/matter/plugs/${p.node_id}/power`, { action, seconds: 10 }),
    action === 'cycle' ? `${p.name}: off, back on in 10 seconds` : `${p.name} turned ${action}`)) {
    setTimeout(done, 800);
  }
}

function PlugState({ plug }: { plug: any }) {
  if (!plug) return <Badge tone="neutral">Not on the Matter server</Badge>;
  if (!plug.available) return <Badge tone="critical">Offline</Badge>;
  if (plug.on == null) return <Badge tone="neutral">No switch</Badge>;
  return plug.on ? <Badge tone="good">On</Badge> : <Badge tone="warning">Off</Badge>;
}

function PlugPower({ plug }: { plug: any }) {
  if (!plug) return <span class="muted">—</span>;
  if (plug.watts == null) return <span class="muted small" title="This plug doesn't measure power (e.g. Linkind / AiDot plugs are on/off only)">no power meter</span>;
  return (
    <span>
      <strong>{fmtW(plug.watts)}</strong>
      {(plug.volts != null || plug.amps != null) && <div class="muted small">{plug.volts != null ? Math.round(plug.volts) + ' V' : ''}{plug.amps != null ? ` · ${plug.amps.toFixed(2)} A` : ''}</div>}
    </span>
  );
}

/** Settings → Smart plugs. */
export function SmartPlugsSettings() {
  const { data, reload } = useFetch<any>('/matter', [], 5000);
  const devices = useFetch<any[]>('/devices');
  const conns = useFetch<any>('/vsphere/connections');
  const [hosts, setHosts] = useState<{ conn: string; connName: string; ref: string; name: string }[]>([]);
  const [url, setUrl] = useState('');
  const [enabled, setEnabled] = useState(false);
  const [busy, setBusy] = useState(false);
  const [code, setCode] = useState('');
  const [name, setName] = useState('');
  const [pairing, setPairing] = useState(false);
  const [editing, setEditing] = useState<{ id: number; name: string } | null>(null);

  useEffect(() => { if (data) { setUrl(data.url); setEnabled(data.enabled); } }, [data?.url, data?.enabled]);
  // every vSphere host, for the "powers" choice
  const connList: any[] = Array.isArray(conns.data) ? conns.data : conns.data?.connections || [];
  useEffect(() => {
    let stop = false;
    Promise.all(connList.map((c) => api.get<any>(`/vsphere/connections/${c.id}/inventory`).then((r) =>
      (r.inventory?.hosts || []).map((h: any) => ({ conn: c.id, connName: c.name, ref: h.ref, name: h.name }))).catch(() => [])))
      .then((all) => { if (!stop) setHosts(all.flat()); });
    return () => { stop = true; };
  }, [connList.map((c) => c.id).join()]);

  if (!data) return <Spinner />;
  const st = data.status || { state: 'off' };
  const [tone, label] = STATE[st.state] || STATE.off;

  const saveSettings = async () => {
    setBusy(true);
    await attempt(async () => { await api.put('/matter/settings', { enabled, url }); await reload(true); }, enabled ? 'Matter turned on' : 'Matter turned off');
    setBusy(false);
  };
  const pair = async () => {
    setPairing(true);
    const ok = await attempt(() => api.post('/matter/plugs', { code, name }), 'Plug added');
    setPairing(false);
    if (ok) { setCode(''); setName(''); reload(true); }
  };
  const assign = async (p: any, v: string) => {
    const body = v === '' ? { device_id: '' } : v.startsWith('d:') ? { device_id: v.slice(2) } : { vs_conn: v.slice(2).split('|')[0], host_ref: v.slice(2).split('|')[1] };
    if (await attempt(() => api.patch(`/matter/plugs/${p.node_id}`, body), v ? 'Plug assigned' : 'Plug unassigned')) reload(true);
  };
  const rename = async () => {
    if (!editing) return;
    const e = editing;
    setEditing(null);
    const p = data.plugs.find((x: any) => x.node_id === e.id);
    if (!p || !e.name.trim() || e.name.trim() === p.name) return;
    if (await attempt(() => api.patch(`/matter/plugs/${e.id}`, { name: e.name.trim() }), 'Renamed')) reload(true);
  };
  const remove = async (p: any) => {
    if (!(await confirmDialog({ title: `Remove ${p.name}?`, body: 'The plug is removed from Apex and from the Matter server. It keeps working in other apps it is paired with.', confirm: 'Remove', danger: true }))) return;
    if (await attempt(() => api.del(`/matter/plugs/${p.node_id}`), 'Plug removed')) reload(true);
  };

  return (
    <div class="stack">
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

      <Card title="Add a plug" icon="plus">
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

      <Card title="Smart plugs" icon="bolt" pad={false}>
        {!data.plugs.length ? <Empty icon="bolt" title="No plugs yet">Pair a Matter plug above.</Empty> : (
          <div class="table-wrap">
            <table class="table">
              <thead><tr><th>Plug</th><th>State</th><th>Power</th><th>Powers</th><th /></tr></thead>
              <tbody>
                {data.plugs.map((p: any) => {
                  const pl = p.plug;
                  const value = p.device_id ? 'd:' + p.device_id : p.vs_conn ? `h:${p.vs_conn}|${p.host_ref}` : '';
                  return (
                    <tr key={p.node_id}>
                      <td>
                        {editing?.id === p.node_id
                          ? <input class="inline-edit" value={editing?.name} autoFocus onInput={(e) => setEditing({ id: p.node_id, name: (e.target as HTMLInputElement).value })}
                              onBlur={rename} onKeyDown={(e) => { if (e.key === 'Enter') rename(); if (e.key === 'Escape') setEditing(null); }} />
                          : <button class="linklike strong" onClick={() => setEditing({ id: p.node_id, name: p.name })} title="Rename">{p.name} <Icon name="edit" size={12} /></button>}
                        <div class="muted small">{pl ? [pl.vendor, pl.product].filter(Boolean).join(' ') : 'node ' + p.node_id}</div>
                      </td>
                      <td><PlugState plug={pl} /></td>
                      <td class="nowrap"><PlugPower plug={pl} /></td>
                      <td>
                        <select value={value} onChange={(e) => assign(p, (e.target as HTMLSelectElement).value)} aria-label="Powers">
                          <option value="">Not assigned</option>
                          {hosts.length > 0 && <optgroup label="ESXi hosts">{hosts.map((h) => <option key={h.conn + h.ref} value={`h:${h.conn}|${h.ref}`}>{h.name}{connList.length > 1 ? ` (${h.connName})` : ''}</option>)}</optgroup>}
                          <optgroup label="Devices">{(devices.data || []).map((d) => <option key={d.id} value={'d:' + d.id}>{d.display_name || d.hostname}</option>)}</optgroup>
                        </select>
                        {pl && pl.watts == null && value && <div class="muted small">State only: this plug doesn't measure power</div>}
                      </td>
                      <td class="nowrap right">
                        {pl?.on === false
                          ? <Button size="sm" icon="power" onClick={() => plugPower(p, 'on', () => reload(true))}>Turn on</Button>
                          : <><Button size="sm" icon="refresh" disabled={!pl?.available} onClick={() => plugPower(p, 'cycle', () => reload(true))}>Power cycle</Button>
                            <Button size="sm" variant="ghost" disabled={!pl?.available} onClick={() => plugPower(p, 'off', () => reload(true))}>Off</Button></>}
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
      <p class="muted small">A plug that measures power replaces the device's or host's own power reading (marked “Matter smart plug”). Plugs without a power meter, like Linkind's, can still power cycle what they're assigned to.</p>
    </div>
  );
}

/** The plug card on a device's Power tab. */
export function DevicePlugCard({ deviceId }: { deviceId: string }) {
  const admin = useCan('admin');
  const { data, reload } = useFetch<any>(`/matter?device=${deviceId}`, [deviceId], 10000);
  if (!data || (!data.enabled && !admin)) return null;
  const p = data.plugs?.[0];
  if (!p) {
    if (!admin || !data.enabled) return null;
    return (
      <Card title="Smart plug" icon="bolt">
        <p class="muted small" style={{ margin: 0 }}>No Matter plug powers this device. <Link href="/settings?tab=plugs">Assign one in Settings → Smart plugs</Link>.</p>
      </Card>
    );
  }
  const pl = p.plug;
  return (
    <Card title="Smart plug" icon="bolt" actions={admin && <Link href="/settings?tab=plugs" class="link">Manage</Link>}>
      <div class="plug-row">
        <div>
          <div class="strong">{p.name}</div>
          <div class="muted small">{pl ? [pl.vendor, pl.product].filter(Boolean).join(' ') : 'Matter plug'}</div>
        </div>
        <PlugState plug={pl} />
        <PlugPower plug={pl} />
      </div>
      {admin && pl && (
        <div class="form-actions">
          {pl.on === false
            ? <Button size="sm" icon="power" onClick={() => plugPower(p, 'on', () => reload(true))}>Turn on</Button>
            : <><Button size="sm" icon="refresh" disabled={!pl.available} onClick={() => plugPower(p, 'cycle', () => reload(true))}>Power cycle</Button>
              <Button size="sm" variant="ghost" disabled={!pl.available} onClick={() => plugPower(p, 'off', () => reload(true))}>Turn off</Button></>}
        </div>
      )}
      {data.status?.state !== 'connected' && <p class="muted small">The Matter server isn't connected; the plug's state may be out of date.</p>}
    </Card>
  );
}
