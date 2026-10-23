import { useEffect, useState } from 'preact/hooks';
import { api } from '../api';
import { Icon } from '../icons';
import { Badge, Button, Card, Empty, ErrorBox, Field, Kv, Modal, Spinner, Tabs, attempt, confirmDialog, fmtBytes, fmtDate, timeAgo, useFetch } from '../ui';
import { useCan } from '../user';

// Managing one vSphere VM: power, CPU / memory, name and notes, disks, network adapters,
// CD/DVD (ISO) and snapshots. Used from Virtualization, a device's "Virtual machine" tab and the
// remote viewer. Reading is open to everyone; power needs a technician, changes an admin.

export const powerTone = (s: string) => (s === 'poweredOn' ? 'good' : s === 'suspended' ? 'warning' : 'neutral');
export const powerLabel = (s: string) => ({ poweredOn: 'On', poweredOff: 'Off', suspended: 'Suspended' } as Record<string, string>)[s] || s;
const fmtMB = (mb: number) => (mb >= 1024 ? +(mb / 1024).toFixed(2) + ' GB' : mb + ' MB');
const gb = (bytes: number) => +(bytes / 2 ** 30).toFixed(2);

export const POWER: { id: string; label: string; icon: string; needs: 'off' | 'on' | 'tools'; danger?: boolean; hint: string }[] = [
  { id: 'on', label: 'Power on', icon: 'play', needs: 'off', hint: 'Starts the VM.' },
  { id: 'shutdown', label: 'Shut down guest', icon: 'power', needs: 'tools', hint: 'Clean shutdown through VMware Tools.' },
  { id: 'reboot', label: 'Restart guest', icon: 'refresh', needs: 'tools', hint: 'Clean restart through VMware Tools.' },
  { id: 'suspend', label: 'Suspend', icon: 'clock', needs: 'on', hint: 'Saves the VM state to disk and stops it.' },
  { id: 'reset', label: 'Reset (hard)', icon: 'refresh', needs: 'on', danger: true, hint: 'Like pressing the reset button: no clean shutdown, unsaved data is lost.' },
  { id: 'off', label: 'Power off (hard)', icon: 'power', needs: 'on', danger: true, hint: 'Like pulling the plug: no clean shutdown, unsaved data is lost.' },
];

const toolsOk = (t: string) => t === 'guestToolsRunning' || t === 'guestToolsExecutingScripts';

export function powerEnabled(a: (typeof POWER)[number], state: string, tools: string) {
  return a.needs === 'off' ? state !== 'poweredOn' : state === 'poweredOn' && (a.needs !== 'tools' || toolsOk(tools));
}

/** Runs a power action after confirming it. */
export async function runPower(connId: string, ref: string, name: string, a: (typeof POWER)[number]) {
  if (!(await confirmDialog({ title: `${a.label}: ${name}?`, body: a.hint, confirm: a.label, danger: !!a.danger }))) return false;
  return attempt(() => api.post(`/vsphere/connections/${connId}/vms/${ref}/power`, { action: a.id }), `${a.label} sent to ${name}`);
}

/** The power menu as a dialog (Virtualization → VMs → Power). */
export function PowerModal({ conn, vm, onClose, onDone }: { conn: any; vm: any; onClose: () => void; onDone: () => void }) {
  const [busy, setBusy] = useState('');
  const run = async (a: (typeof POWER)[number]) => {
    setBusy(a.id);
    if (await runPower(conn.id, vm.ref, vm.name, a)) onDone();
    setBusy('');
  };
  return (
    <Modal title={<>Power: {vm.name}</>} onClose={onClose} width={420}>
      <div class="menu" style="position:static;box-shadow:none;border:0;padding:0;min-width:0">
        {POWER.map((a) => (
          <button key={a.id} class={a.danger ? 'danger' : ''} disabled={!powerEnabled(a, vm.power_state, vm.tools_running) || !!busy} onClick={() => run(a)}
            title={!powerEnabled(a, vm.power_state, vm.tools_running) && a.needs === 'tools' && vm.power_state === 'poweredOn' ? 'VMware Tools is not running' : a.hint}>
            {busy === a.id ? <span class="spinner sm" /> : <Icon name={a.icon} size={16} />} {a.label}
          </button>
        ))}
      </div>
      <p class="muted small">Currently {powerLabel(vm.power_state).toLowerCase()}{vm.power_state === 'poweredOn' && !toolsOk(vm.tools_running) ? ' · VMware Tools is not running, so only the hard actions are available' : ''}. Actions are recorded in the audit log.</p>
    </Modal>
  );
}

/** The whole VM management panel. */
export function VMPanel({ connId, vmRef, vm, onChanged, initialTab = 'summary' }: { connId: string; vmRef: string; vm?: any; onChanged?: () => void; initialTab?: string }) {
  const base = `/vsphere/connections/${connId}/vms/${vmRef}`;
  const { data, error, reload } = useFetch<any>(base + '/config', [connId, vmRef]);
  const [tab, setTab] = useState(initialTab);
  const admin = useCan('admin');
  const tech = useCan('technician');
  if (!data) return error ? <ErrorBox msg={error} /> : <Spinner label="Reading the VM from vSphere…" />;
  const c = data.config;
  const changed = () => { reload(true); onChanged?.(); };
  const tabs = [
    { id: 'summary', label: 'Summary', icon: 'info' },
    { id: 'hardware', label: 'CPU & memory', icon: 'cpu' },
    { id: 'disks', label: 'Disks', icon: 'disk', count: c.disks.length },
    { id: 'network', label: 'Network', icon: 'network', count: c.nics.length },
    { id: 'cd', label: 'CD/DVD', icon: 'download' },
    { id: 'snapshots', label: 'Snapshots', icon: 'history', count: c.snapshots.length },
  ];
  const p = { base, connId, c, admin, tech, changed, vm };
  return (
    <div class="vm-panel">
      <Tabs tabs={tabs} active={tab} onChange={setTab} />
      {c.template && <p class="callout warn small"><Icon name="info" size={14} /><span>This is a template; convert it to a VM in vSphere to change it.</span></p>}
      {tab === 'summary' && <Summary {...p} />}
      {tab === 'hardware' && <Hardware {...p} />}
      {tab === 'disks' && <Disks {...p} />}
      {tab === 'network' && <Network {...p} />}
      {tab === 'cd' && <Cdroms {...p} />}
      {tab === 'snapshots' && <Snapshots {...p} />}
      {!admin && tab !== 'summary' && <p class="muted small">Only admins can change VM settings.</p>}
    </div>
  );
}

type P = { base: string; connId: string; c: any; admin: boolean; tech: boolean; changed: () => void; vm?: any };

function Summary({ connId, c, tech, changed, vm }: P) {
  const [busy, setBusy] = useState('');
  const run = async (a: (typeof POWER)[number]) => {
    setBusy(a.id);
    if (await runPower(connId, c.ref, c.name, a)) setTimeout(changed, 2500);
    setBusy('');
  };
  return (
    <div class="stack">
      <Kv items={[
        ['Name', <strong>{c.name}</strong>],
        ['Power', <Badge tone={powerTone(c.power_state)}>{powerLabel(c.power_state)}</Badge>],
        ['Guest OS', c.guest_os || '—'],
        ['VMware Tools', toolsOk(c.tools_running) ? 'running' : 'not running'],
        ['Resources', `${c.cpus} vCPU · ${fmtMB(c.mem_mb)} · ${c.disks.length} disk${c.disks.length === 1 ? '' : 's'} (${fmtBytes(c.disks.reduce((s: number, d: any) => s + d.size_bytes, 0), 0)})`],
        ...(vm?.host_name ? [['Host', vm.host_name] as [string, any]] : []),
        ...(vm?.ips?.length ? [['IP addresses', vm.ips.join(', ')] as [string, any]] : []),
        ['Hardware version', c.hw_version || '—'],
        ['Snapshots', c.snapshots.length ? `${c.snapshots.length} (oldest ${timeAgo(c.snapshots.reduce((a: any, s: any) => (!a || s.created < a ? s.created : a), ''))})` : 'none'],
      ]} />
      {c.annotation && <p class="vm-notes">{c.annotation}</p>}
      {tech && !c.template && (
        <div class="vm-power">
          {POWER.map((a) => (
            <Button key={a.id} size="sm" variant={a.danger ? 'danger' : 'default'} icon={a.icon} loading={busy === a.id}
              disabled={!powerEnabled(a, c.power_state, c.tools_running) || !!busy} title={a.hint} onClick={() => run(a)}>{a.label}</Button>
          ))}
        </div>
      )}
      {vm?.ui_url && <a class="link small" href={vm.ui_url} target="_blank" rel="noopener noreferrer"><Icon name="external" size={13} /> Open in the vSphere client (web console)</a>}
    </div>
  );
}

function Hardware({ base, c, admin, changed }: P) {
  const [cpus, setCpus] = useState(c.cpus);
  const [cps, setCps] = useState(c.cores_per_socket);
  const [memGB, setMemGB] = useState(+(c.mem_mb / 1024).toFixed(2));
  const [name, setName] = useState(c.name);
  const [notes, setNotes] = useState(c.annotation || '');
  const [busy, setBusy] = useState(false);
  useEffect(() => { setCpus(c.cpus); setCps(c.cores_per_socket); setMemGB(+(c.mem_mb / 1024).toFixed(2)); setName(c.name); setNotes(c.annotation || ''); }, [c]);
  const on = c.power_state === 'poweredOn';
  const divisors = Array.from({ length: Math.max(1, cpus) }, (_, i) => i + 1).filter((n) => cpus % n === 0);
  const memMB = Math.round((memGB * 1024) / 4) * 4;
  const dirty = cpus !== c.cpus || cps !== c.cores_per_socket || memMB !== c.mem_mb || name.trim() !== c.name || notes !== (c.annotation || '');
  const save = async () => {
    const body: any = {};
    if (cpus !== c.cpus) body.cpus = cpus;
    if (cps !== c.cores_per_socket) body.cores_per_socket = cps;
    if (memMB !== c.mem_mb) body.mem_mb = memMB;
    if (name.trim() !== c.name) body.name = name.trim();
    if (notes !== (c.annotation || '')) body.annotation = notes;
    if ((body.cpus && body.cpus < c.cpus) || (body.mem_mb && body.mem_mb < c.mem_mb)) {
      if (!(await confirmDialog({ title: 'Reduce resources?', body: 'The guest gets fewer vCPUs or less memory the next time it starts. Make sure what runs on it still fits.', confirm: 'Save' }))) return;
    }
    setBusy(true);
    if (await attempt(() => api.patch(base + '/config', body), 'VM settings saved')) changed();
    setBusy(false);
  };
  const ro = !admin || c.template;
  return (
    <div class="stack">
      {on && (!c.cpu_hot_add || !c.mem_hot_add) && (
        <p class="callout small"><Icon name="info" size={14} /><span>The VM is running.{!c.cpu_hot_add && ' CPU hot add is off, so vCPUs can only be changed while it is powered off.'}{!c.mem_hot_add && ' Memory hot add is off, so memory can only be changed while it is powered off.'} Removing CPU or memory always needs it powered off.</span></p>
      )}
      <div class="form-grid">
        <Field label="vCPUs" hint={c.max_cpus ? `The host has ${c.max_cpus} logical CPUs` : undefined}>
          <input type="number" min={1} max={c.max_cpus || 768} value={cpus} disabled={ro} onInput={(e) => { const v = Math.max(1, +(e.target as HTMLInputElement).value || 1); setCpus(v); if (v % cps) setCps(1); }} />
        </Field>
        <Field label="Cores per socket" hint={`${cpus / cps} socket${cpus / cps === 1 ? '' : 's'}${on ? ' · change while powered off' : ''}`}>
          <select value={cps} disabled={ro || on} onChange={(e) => setCps(+(e.target as HTMLSelectElement).value)}>
            {divisors.map((d) => <option key={d} value={d}>{d}</option>)}
          </select>
        </Field>
        <Field label="Memory (GB)" hint={`${memMB} MB${c.max_mem_mb ? ` · host has ${Math.round(c.max_mem_mb / 1024)} GB` : ''}`}>
          <input type="number" min={0.25} step={0.25} value={memGB} disabled={ro} onInput={(e) => setMemGB(+(e.target as HTMLInputElement).value)} />
        </Field>
        <Field label="VM name" hint="The name in vSphere (not the guest's computer name)">
          <input value={name} disabled={ro} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
        </Field>
      </div>
      <Field label="Notes">
        <textarea rows={3} value={notes} disabled={ro} onInput={(e) => setNotes((e.target as HTMLTextAreaElement).value)} />
      </Field>
      {!ro && <div class="form-actions"><span class="grow" /><Button variant="primary" loading={busy} disabled={!dirty} onClick={save}>Save changes</Button></div>}
    </div>
  );
}

function Disks({ base, c, admin, changed }: P) {
  const [grow, setGrow] = useState<{ key: number; gb: number } | null>(null);
  const [add, setAdd] = useState(false);
  const [remove, setRemove] = useState<any | null>(null);
  const [busy, setBusy] = useState(false);
  const doGrow = async () => {
    if (!grow) return;
    setBusy(true);
    if (await attempt(() => api.patch(`${base}/disks/${grow.key}`, { gb: grow.gb }), 'Disk enlarged')) { setGrow(null); changed(); }
    setBusy(false);
  };
  const ro = !admin || c.template;
  return (
    <div class="stack">
      <div class="table-wrap">
        <table class="table">
          <thead><tr><th>Disk</th><th>Size</th><th class="hide-sm">Datastore</th><th class="hide-sm">Type</th><th /></tr></thead>
          <tbody>
            {c.disks.map((d: any) => (
              <tr key={d.key}>
                <td><span class="strong">{d.label}</span><div class="muted small mono truncate" title={d.file} style="max-width:260px">{d.unit} · {d.file}</div></td>
                <td class="nowrap">
                  {grow?.key === d.key
                    ? <span class="inline-form"><input type="number" class="w-num" min={gb(d.size_bytes)} step={1} value={grow!.gb} onInput={(e) => setGrow({ key: d.key, gb: +(e.target as HTMLInputElement).value })} /> GB</span>
                    : fmtBytes(d.size_bytes, 1)}
                </td>
                <td class="hide-sm small">{d.datastore || '—'}</td>
                <td class="hide-sm small">{d.thin ? 'Thin' : 'Thick'}</td>
                <td class="right nowrap">
                  {!ro && (grow?.key === d.key
                    ? <><Button size="sm" variant="primary" loading={busy} disabled={grow!.gb <= gb(d.size_bytes)} onClick={doGrow}>Save</Button><Button size="sm" variant="ghost" onClick={() => setGrow(null)}>Cancel</Button></>
                    : <><Button size="sm" variant="ghost" icon="up" onClick={() => setGrow({ key: d.key, gb: Math.ceil(gb(d.size_bytes)) + 10 })}>Grow</Button>
                      <Button size="sm" variant="ghost" icon="trash" title="Remove disk" onClick={() => setRemove(d)} /></>)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {!ro && <p class="muted small">Disks can be made larger, not smaller. After growing a disk, extend the partition inside the guest (Disk Management on Windows, growpart / resize2fs on Linux). A disk can't be grown while the VM has snapshots.</p>}
      {!ro && !add && <div><Button icon="plus" onClick={() => setAdd(true)}>Add disk</Button></div>}
      {add && <AddDisk base={base} c={c} onClose={() => setAdd(false)} onDone={() => { setAdd(false); changed(); }} />}
      {remove && <RemoveDisk base={base} disk={remove} onClose={() => setRemove(null)} onDone={() => { setRemove(null); changed(); }} />}
    </div>
  );
}

function AddDisk({ base, c, onClose, onDone }: { base: string; c: any; onClose: () => void; onDone: () => void }) {
  const [size, setSize] = useState(50);
  const [ds, setDs] = useState(() => {
    const cur = c.datastores.find((d: any) => d.name === c.disks[0]?.datastore);
    return (cur || c.datastores[0])?.ref || '';
  });
  const [thin, setThin] = useState(true);
  const [busy, setBusy] = useState(false);
  const sel = c.datastores.find((d: any) => d.ref === ds);
  const save = async () => {
    setBusy(true);
    if (await attempt(() => api.post(base + '/disks', { gb: size, datastore: ds, thin }), 'Disk added')) onDone();
    setBusy(false);
  };
  return (
    <Card title="New disk" icon="disk">
      <div class="form-grid">
        <Field label="Size (GB)"><input type="number" min={1} value={size} onInput={(e) => setSize(+(e.target as HTMLInputElement).value)} /></Field>
        <Field label="Datastore" hint={sel ? `${fmtBytes(sel.free, 0)} free of ${fmtBytes(sel.capacity, 0)}` : undefined}>
          <select value={ds} onChange={(e) => setDs((e.target as HTMLSelectElement).value)}>
            {c.datastores.map((d: any) => <option key={d.ref} value={d.ref}>{d.name} ({fmtBytes(d.free, 0)} free)</option>)}
          </select>
        </Field>
      </div>
      <label class="check"><input type="checkbox" checked={thin} onChange={(e) => setThin((e.target as HTMLInputElement).checked)} /> Thin provisioned (uses space as data is written)</label>
      {!thin && sel && size * 2 ** 30 > sel.free && <p class="small text-warn">The datastore doesn't have room for a {size} GB thick disk.</p>}
      <div class="form-actions"><span class="grow" /><Button onClick={onClose}>Cancel</Button><Button variant="primary" icon="plus" loading={busy} disabled={size <= 0 || !ds} onClick={save}>Add disk</Button></div>
    </Card>
  );
}

function RemoveDisk({ base, disk, onClose, onDone }: { base: string; disk: any; onClose: () => void; onDone: () => void }) {
  const [del, setDel] = useState(false);
  const [busy, setBusy] = useState(false);
  const go = async () => {
    setBusy(true);
    if (await attempt(() => api.del(`${base}/disks/${disk.key}${del ? '?delete_files=1' : ''}`), del ? 'Disk removed and deleted' : 'Disk removed')) onDone();
    setBusy(false);
  };
  return (
    <Modal title={`Remove ${disk.label}?`} onClose={onClose} width={460}
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="danger" loading={busy} onClick={go}>{del ? 'Remove and delete' : 'Remove'}</Button></>}>
      <p class="muted">The guest loses this disk ({fmtBytes(disk.size_bytes, 1)}, {disk.file}). Make sure nothing on it is needed.</p>
      <label class="check"><input type="checkbox" checked={del} onChange={(e) => setDel((e.target as HTMLInputElement).checked)} /> Also delete the disk file from the datastore (can't be undone)</label>
      {!del && <p class="muted small">Without this the file stays on the datastore and can be attached again in vSphere.</p>}
    </Modal>
  );
}

function Network({ base, c, admin, changed }: P) {
  const [busy, setBusy] = useState(0);
  const set = async (n: any, body: any, msg: string, confirm?: string) => {
    if (confirm && !(await confirmDialog({ title: confirm, body: 'The guest loses its network connection on this adapter until it is moved back. If you are connected through it (remote control, the Apex agent) that connection drops.', confirm: 'Continue', danger: true }))) return;
    setBusy(n.key);
    if (await attempt(() => api.patch(`${base}/nics/${n.key}`, body), msg)) changed();
    setBusy(0);
  };
  const ro = !admin || c.template;
  const on = c.power_state === 'poweredOn';
  if (!c.nics.length) return <Empty icon="network" title="No network adapters" />;
  return (
    <div class="table-wrap">
      <table class="table">
        <thead><tr><th>Adapter</th><th>Network</th><th>Connected</th></tr></thead>
        <tbody>
          {c.nics.map((n: any) => (
            <tr key={n.key}>
              <td><span class="strong">{n.label}</span><div class="muted small mono">{n.type} · {n.mac}</div></td>
              <td>
                {ro ? n.network || '—' : (
                  <select value={n.network_ref} disabled={busy === n.key} onChange={(e) => {
                    const v = (e.target as HTMLSelectElement).value;
                    const name = c.networks.find((x: any) => x.ref === v)?.name || v;
                    set(n, { network: v }, `${n.label} moved to ${name}`, on ? `Move ${n.label} to ${name}?` : undefined);
                  }}>
                    {!c.networks.some((x: any) => x.ref === n.network_ref) && <option value={n.network_ref}>{n.network || 'unknown'}</option>}
                    {c.networks.map((x: any) => <option key={x.ref} value={x.ref}>{x.name}</option>)}
                  </select>
                )}
              </td>
              <td class="nowrap">
                <Badge tone={n.connected ? 'good' : on ? 'warning' : 'neutral'}>{n.connected ? 'Connected' : on ? 'Disconnected' : n.start_connected ? 'At power on' : 'Disconnected'}</Badge>
                {!ro && (n.connected || (!on && n.start_connected)
                  ? <Button size="sm" variant="ghost" loading={busy === n.key} onClick={() => set(n, { connected: false }, `${n.label} disconnected`, on ? `Disconnect ${n.label}?` : undefined)}>Disconnect</Button>
                  : <Button size="sm" variant="ghost" loading={busy === n.key} onClick={() => set(n, { connected: true }, `${n.label} connected`)}>Connect</Button>)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Cdroms({ base, connId, c, admin, changed }: P) {
  const [busy, setBusy] = useState(0);
  const [pick, setPick] = useState<any | null>(null);
  const ro = !admin || c.template;
  const eject = async (d: any) => {
    setBusy(d.key);
    if (await attempt(() => api.patch(`${base}/cdroms/${d.key}`, { iso: '' }), 'ISO ejected')) changed();
    setBusy(0);
  };
  if (!c.cdroms.length) return <Empty icon="download" title="No CD/DVD drive">Add one in vSphere to mount ISO images.</Empty>;
  return (
    <div class="stack">
      {c.cdroms.map((d: any) => (
        <div class="vm-cd" key={d.key}>
          <Icon name="download" />
          <div class="grow">
            <div class="strong">{d.label}</div>
            <div class="muted small mono">{d.iso || 'No ISO mounted'}{d.iso && (d.connected ? ' · connected' : d.start_connected ? ' · connects at power on' : ' · not connected')}</div>
          </div>
          {!ro && <>
            <Button size="sm" icon="folder" onClick={() => setPick(d)}>{d.iso ? 'Change ISO' : 'Mount ISO'}</Button>
            {d.iso && <Button size="sm" variant="ghost" loading={busy === d.key} onClick={() => eject(d)}>Eject</Button>}
          </>}
        </div>
      ))}
      {pick && <ISOPicker base={base} connId={connId} c={c} drive={pick} onClose={() => setPick(null)} onDone={() => { setPick(null); changed(); }} />}
    </div>
  );
}

function ISOPicker({ base, connId, c, drive, onClose, onDone }: { base: string; connId: string; c: any; drive: any; onClose: () => void; onDone: () => void }) {
  const [ds, setDs] = useState(c.datastores[0]?.ref || '');
  const [list, setList] = useState<any[] | null>(null);
  const [err, setErr] = useState('');
  const [q, setQ] = useState('');
  const [busy, setBusy] = useState('');
  useEffect(() => {
    if (!ds) return;
    setList(null);
    setErr('');
    api.get<any[]>(`/vsphere/connections/${connId}/datastores/${ds}/isos`).then(setList).catch((e) => setErr(e.message));
  }, [ds]);
  const mount = async (path: string) => {
    setBusy(path);
    if (await attempt(() => api.patch(`${base}/cdroms/${drive.key}`, { iso: path }), 'ISO mounted')) onDone();
    setBusy('');
  };
  const rows = (list || []).filter((f) => !q || f.path.toLowerCase().includes(q.toLowerCase()));
  return (
    <Modal title={`Mount an ISO: ${drive.label}`} onClose={onClose} width={620}>
      <div class="form-grid">
        <Field label="Datastore">
          <select value={ds} onChange={(e) => setDs((e.target as HTMLSelectElement).value)}>
            {c.datastores.map((d: any) => <option key={d.ref} value={d.ref}>{d.name}</option>)}
          </select>
        </Field>
        <Field label="Filter"><input value={q} placeholder="ubuntu, windows…" onInput={(e) => setQ((e.target as HTMLInputElement).value)} /></Field>
      </div>
      {err ? <ErrorBox msg={err} /> : !list ? <Spinner label="Searching the datastore for .iso files…" /> : !rows.length ? <Empty icon="file" title="No ISO files found">Upload ISOs to the datastore in vSphere first.</Empty> : (
        <div class="vm-iso-list">
          {rows.map((f) => (
            <button key={f.path} class="vm-iso" disabled={!!busy} onClick={() => mount(f.path)}>
              <Icon name="file" size={15} /><span class="grow mono truncate" title={f.path}>{f.path}</span><span class="muted small">{fmtBytes(f.size, 1)}</span>
              {busy === f.path && <span class="spinner sm" />}
            </button>
          ))}
        </div>
      )}
      <p class="muted small">The ISO is connected right away when the VM is running, otherwise at the next power on.</p>
    </Modal>
  );
}

function Snapshots({ base, c, admin, changed }: P) {
  const [busy, setBusy] = useState('');
  const [take, setTake] = useState(false);
  const ro = !admin || c.template;
  const revert = async (s: any) => {
    if (!(await confirmDialog({ title: `Revert to "${s.name}"?`, danger: true, confirm: 'Revert',
      body: `The VM goes back to how it was on ${fmtDate(s.created)}. Everything since then that isn't in another snapshot is lost${s.power_state === 'poweredOn' ? '; the VM resumes running from that point' : '; the VM will be powered off'}.` }))) return;
    setBusy(s.ref);
    if (await attempt(() => api.post(`${base}/snapshots/${s.ref}/revert`, {}), `Reverted to ${s.name}`)) changed();
    setBusy('');
  };
  const del = async (s: any) => {
    if (!(await confirmDialog({ title: `Delete snapshot "${s.name}"?`, danger: true, confirm: 'Delete',
      body: 'Its changes are merged into the VM (nothing in the running VM is lost), and you can no longer go back to it. Merging a large snapshot can take a while.' }))) return;
    setBusy(s.ref);
    if (await attempt(() => api.del(`${base}/snapshots/${s.ref}`), `Snapshot ${s.name} deleted`)) changed();
    setBusy('');
  };
  return (
    <div class="stack">
      {!c.snapshots.length ? <Empty icon="history" title="No snapshots" /> : (
        <div class="vm-snaps">
          {c.snapshots.map((s: any) => (
            <div key={s.ref} class={'vm-snap' + (s.current ? ' current' : '')} style={{ paddingLeft: 10 + s.depth * 18 + 'px' }}>
              <Icon name={s.power_state === 'poweredOn' ? 'memory' : 'history'} size={15} />
              <div class="grow">
                <span class="strong">{s.name}</span>{s.current && <Badge tone="info">you are here</Badge>}
                <div class="muted small">{fmtDate(s.created)} · {timeAgo(s.created)}{s.power_state === 'poweredOn' ? ' · includes memory' : ''}{s.quiesced ? ' · quiesced' : ''}{s.description ? ' · ' + s.description : ''}</div>
              </div>
              {!ro && <>
                <Button size="sm" variant="ghost" icon="history" loading={busy === s.ref} disabled={!!busy} onClick={() => revert(s)}>Revert</Button>
                <Button size="sm" variant="ghost" icon="trash" title="Delete snapshot" disabled={!!busy} onClick={() => del(s)} />
              </>}
            </div>
          ))}
        </div>
      )}
      {c.snapshots.some((s: any) => Date.now() - new Date(s.created).getTime() > 3 * 86400e3) && <p class="small text-warn">Snapshots aren't backups: old ones grow and slow the VM down. Delete them once you no longer need to go back.</p>}
      {!ro && (take ? <TakeSnapshot base={base} c={c} onClose={() => setTake(false)} onDone={() => { setTake(false); changed(); }} /> : <div><Button icon="plus" onClick={() => setTake(true)}>Take snapshot</Button></div>)}
    </div>
  );
}

function TakeSnapshot({ base, c, onClose, onDone }: { base: string; c: any; onClose: () => void; onDone: () => void }) {
  const d = new Date();
  const [name, setName] = useState(`Apex ${d.toISOString().slice(0, 10)} ${d.toTimeString().slice(0, 5)}`);
  const [desc, setDesc] = useState('');
  const on = c.power_state === 'poweredOn';
  const [memory, setMemory] = useState(false);
  const [quiesce, setQuiesce] = useState(on && toolsOk(c.tools_running));
  const [busy, setBusy] = useState(false);
  const save = async () => {
    setBusy(true);
    if (await attempt(() => api.post(base + '/snapshots', { name, description: desc, memory: on && memory, quiesce: on && !memory && quiesce }), 'Snapshot taken')) onDone();
    setBusy(false);
  };
  return (
    <Card title="Take a snapshot" icon="history">
      <div class="form-grid">
        <Field label="Name"><input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} /></Field>
        <Field label="Description"><input value={desc} placeholder="e.g. before Windows updates" onInput={(e) => setDesc((e.target as HTMLInputElement).value)} /></Field>
      </div>
      {on && <>
        <label class="check"><input type="checkbox" checked={memory} onChange={(e) => setMemory((e.target as HTMLInputElement).checked)} /> Include the VM's memory (reverting resumes it running; slower and uses disk space equal to its RAM)</label>
        <label class="check"><input type="checkbox" checked={quiesce && !memory} disabled={memory || !toolsOk(c.tools_running)} onChange={(e) => setQuiesce((e.target as HTMLInputElement).checked)} /> Quiesce the guest file system (needs VMware Tools)</label>
      </>}
      <div class="form-actions">{busy && <span class="muted small">Taking the snapshot…</span>}<span class="grow" /><Button onClick={onClose}>Cancel</Button><Button variant="primary" loading={busy} disabled={!name.trim()} onClick={save}>Take snapshot</Button></div>
    </Card>
  );
}

/** Dialog version, opened from the Virtualization VM list. */
export function VMModal({ connId, vm, onClose, onChanged }: { connId: string; vm: any; onClose: () => void; onChanged?: () => void }) {
  return (
    <Modal title={<>Virtual machine: {vm.name}</>} onClose={onClose} width={900}>
      <VMPanel connId={connId} vmRef={vm.ref} vm={vm} onChanged={onChanged} />
    </Modal>
  );
}

/** The VM an agent device runs in, if Apex knows it (null while loading or when it isn't a known VM). */
export function useDeviceVM(deviceId: string) {
  const { data, reload } = useFetch<any>(`/devices/${deviceId}/vm`, [deviceId], 60000);
  return { vm: data?.vm || null, conn: data?.connection || null, reload };
}

/** Device page → "Virtual machine" tab. */
export function DeviceVMTab({ deviceId }: { deviceId: string }) {
  const { vm, conn, reload } = useDeviceVM(deviceId);
  if (!vm) return <Spinner />;
  return (
    <Card title={<>{vm.name} <span class="muted small">on {vm.host_name || 'vSphere'} · {conn.name}</span></>} icon="box"
      actions={<Badge tone={powerTone(vm.power_state)}>{powerLabel(vm.power_state)}</Badge>}>
      <VMPanel connId={conn.id} vmRef={vm.ref} vm={vm} onChanged={() => setTimeout(() => reload(true), 3000)} />
    </Card>
  );
}
