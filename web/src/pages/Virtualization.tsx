import { Fragment } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import { api } from '../api';
import { Icon } from '../icons';
import { HostEnergy } from './HostEnergy';
import { Link, navigate, useQuery } from '../router';
import { Badge, Button, Card, Empty, ErrorBox, Field, Meter, Modal, PageHeader, SearchInput, Spinner, Tabs, attempt, confirmDialog, fmtBytes, fmtDate, timeAgo, toast, useFetch } from '../ui';
import { useCan } from '../user';
import { LineChart } from '../chart';

// ---------- helpers ----------

const statusTone = (s: string) => (s === 'red' ? 'critical' : s === 'yellow' ? 'warning' : s === 'green' ? 'good' : 'neutral');
const powerTone = (s: string) => (s === 'poweredOn' ? 'good' : s === 'suspended' ? 'warning' : 'neutral');
const powerLabel = (s: string) => ({ poweredOn: 'On', poweredOff: 'Off', suspended: 'Suspended' } as Record<string, string>)[s] || s;
const osName = (f: string) => ({ windows: 'Windows', linux: 'Linux', darwin: 'macOS' } as Record<string, string>)[f] || 'Other';
const fmtMB = (mb: number) => (mb >= 1024 ? (mb / 1024).toFixed(mb % 1024 ? 1 : 0) + ' GB' : mb + ' MB');
const fmtUp = (sec: number) => {
  if (!sec) return '—';
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600);
  return d > 0 ? `${d}d ${h}h` : `${h}h ${Math.floor((sec % 3600) / 60)}m`;
};

/** Why a VM cannot receive the agent through VMware Tools, or '' if it can. */
function deployBlock(vm: any): string {
  if (vm.template) return 'Template';
  if (vm.power_state !== 'poweredOn') return 'Not powered on';
  if (vm.os_family !== 'windows' && vm.os_family !== 'linux') return 'Windows and Linux only';
  if (vm.tools_running !== 'guestToolsRunning' && vm.tools_running !== 'guestToolsExecutingScripts') return 'VMware Tools not running';
  return '';
}

const hostState = (h: any) =>
  h.connection_state === 'notResponding' ? <Badge tone="critical">not responding</Badge>
  : h.connection_state === 'disconnected' ? <Badge tone="warning">disconnected</Badge>
  : h.maintenance ? <Badge tone="warning">maintenance</Badge>
  : <Badge tone="good">connected</Badge>;

// ---------- page ----------

export function Virtualization() {
  const q = useQuery();
  const admin = useCan('admin');
  const tech = useCan('technician');
  const conns = useFetch<any[]>('/vsphere/connections', [], 20000);
  const tab = q.get('tab') || 'vms';
  const connId = q.get('c') || conns.data?.[0]?.id || '';
  const setQ = (k: string, v: string) => {
    const p = new URLSearchParams(location.search);
    if (v) p.set(k, v); else p.delete(k);
    navigate('/virtualization?' + p.toString(), true);
  };

  if (!conns.data) return <div class="page"><PageHeader title="Virtualization" /><Spinner /></div>;

  if (conns.data.length === 0) {
    return (
      <div class="page">
        <PageHeader title="Virtualization" subtitle="Monitor VMware vCenter / ESXi and install the agent into virtual machines." />
        <Card>
          <Empty icon="server" title="No vSphere connection yet">
            <p>Connect a vCenter Server (or a standalone ESXi host) with a read-only account to see hosts, VMs and datastores here, get alerts, and install the Apex agent into VMs.</p>
            {admin ? <p><Button variant="primary" icon="plus" onClick={() => setQ('tab', 'connections')}>Add connection</Button></p> : <p class="muted">Ask an administrator to add one.</p>}
          </Empty>
        </Card>
        {tab === 'connections' && admin && <ConnectionEditor conn={{}} onClose={() => setQ('tab', '')} onSaved={() => { setQ('tab', ''); conns.reload(true); }} />}
      </div>
    );
  }

  const conn = conns.data.find((c) => c.id === connId) || conns.data[0];
  const tabs = [
    { id: 'vms', label: 'Virtual machines', icon: 'box' },
    { id: 'hosts', label: 'Hosts', icon: 'server' },
    { id: 'energy', label: 'Energy', icon: 'bolt' },
    { id: 'datastores', label: 'Datastores', icon: 'disk' },
    { id: 'alarms', label: 'Alarms', icon: 'bell' },
    { id: 'deploys', label: 'Agent deployments', icon: 'download' },
    ...(admin ? [{ id: 'connections', label: 'Connections', icon: 'settings' }] : []),
  ];
  return (
    <div class="page">
      <PageHeader
        title="Virtualization"
        subtitle={
          <>
            {conn.name}
            {conn.status === 'error' ? <> · <span class="text-crit">connection problem</span></> : conn.last_poll ? <> · updated {timeAgo(conn.last_poll)}</> : <> · waiting for the first read…</>}
          </>
        }
        actions={<>
          {conns.data.length > 1 && (
            <select value={conn.id} onChange={(e) => setQ('c', (e.target as HTMLSelectElement).value)}>
              {conns.data.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
            </select>
          )}
        </>}
      />
      {conn.status === 'error' && tab !== 'connections' && (
        <ErrorBox msg={admin && conn.last_error ? `Cannot read ${conn.name}: ${conn.last_error}` : `Cannot read ${conn.name} right now. Ask an administrator to check the connection.`} />
      )}
      <Tabs tabs={tabs} active={tab} onChange={(t) => setQ('tab', t)} />
      {tab === 'connections' && admin
        ? <Connections conns={conns.data} reload={() => conns.reload(true)} />
        : <Inventory key={conn.id} conn={conn} tab={tab} tech={tech} admin={admin} onConn={() => conns.reload(true)} />}
    </div>
  );
}

// ---------- inventory ----------

function Inventory({ conn, tab, tech, admin, onConn }: { conn: any; tab: string; tech: boolean; admin: boolean; onConn: () => void }) {
  const { data, error, loading, reload } = useFetch<any>(`/vsphere/connections/${conn.id}/inventory`, [], 15000);
  const [busy, setBusy] = useState(false);
  const inv = data?.inventory;

  const refresh = async () => {
    setBusy(true);
    try {
      await api.post(`/vsphere/connections/${conn.id}/refresh`);
      await reload(true);
      onConn();
    } catch (e: any) { toast(e.message, 'error'); }
    setBusy(false);
  };

  if (tab === 'deploys') return <Deploys conn={conn} />;
  if (tab === 'energy') return <HostEnergy conn={conn} />;
  if (error) return <ErrorBox msg={error} />;
  if (loading && !data) return <Spinner />;
  if (!inv) {
    return <Card><Empty icon="server" title="Reading the inventory…">The first read usually takes a few seconds. {tech && <p><Button icon="refresh" onClick={refresh} loading={busy}>Read now</Button></p>}</Empty></Card>;
  }
  const hosts: any[] = inv.hosts, vms: any[] = inv.vms, dss: any[] = inv.datastores, alarms: any[] = inv.alarms;
  const on = vms.filter((v) => v.power_state === 'poweredOn' && !v.template).length;
  const total = vms.filter((v) => !v.template).length;
  const hostProblems = hosts.filter((h) => h.connection_state !== 'connected' || h.status === 'red' || h.sensors?.length).length;
  const worstDs = dss.reduce((m, d) => (d.capacity > 0 ? Math.max(m, d.used_pct) : m), 0);
  const withAgent = vms.filter((v) => v.agent).length;
  return (
    <>
      <div class="stats small-stats">
        <div class={'stat ' + (hostProblems ? 'stat-warning' : '')}><div class="stat-icon"><Icon name="server" /></div><div><div class="stat-label">Hosts</div><div class="stat-value">{hosts.length}</div><div class="stat-sub">{hostProblems ? `${hostProblems} need attention` : 'all healthy'}</div></div></div>
        <div class="stat"><div class="stat-icon"><Icon name="box" /></div><div><div class="stat-label">Virtual machines</div><div class="stat-value">{on}<small> / {total} on</small></div><div class="stat-sub">{withAgent} with an Apex agent</div></div></div>
        <div class={'stat ' + (worstDs >= conn.ds_crit_pct ? 'stat-critical' : worstDs >= conn.ds_warn_pct ? 'stat-warning' : '')}><div class="stat-icon"><Icon name="disk" /></div><div><div class="stat-label">Datastores</div><div class="stat-value">{dss.length}</div><div class="stat-sub">fullest {Math.round(worstDs)}%</div></div></div>
        {inv.power?.hosts_reporting > 0 && <div class="stat"><div class="stat-icon"><Icon name="power" /></div><div><div class="stat-label">Host power draw</div><div class="stat-value">{Math.round(inv.power.total_watts)}<small> W</small></div><div class="stat-sub">≈ {inv.power.kwh_per_day.toFixed(1)} kWh/day · {inv.power.hosts_reporting} of {hosts.length} hosts reporting</div></div></div>}
        <Link href="/alerts" class={'stat ' + (conn.open_alerts ? 'stat-warning' : '')}><div class="stat-icon"><Icon name="bell" /></div><div><div class="stat-label">Open alerts</div><div class="stat-value">{conn.open_alerts || 0}</div><div class="stat-sub">{alarms.length} vCenter alarms</div></div></Link>
      </div>
      {tab === 'hosts' && <Hosts hosts={hosts} connId={conn.id} />}
      {tab === 'vms' && <VMs conn={conn} vms={vms} tech={tech} admin={admin} reload={() => reload(true)} refresh={refresh} busy={busy} />}
      {tab === 'datastores' && <Datastores dss={dss} conn={conn} />}
      {tab === 'alarms' && <Alarms alarms={alarms} />}
      <p class="muted small">vSphere {inv.about.version} (build {inv.about.build}) · read {fmtDate(inv.collected_at)} · refreshed every {conn.poll_seconds}s
        {tech && <> · <button class="linklike" onClick={refresh} disabled={busy}>{busy ? 'refreshing…' : 'refresh now'}</button></>}</p>
    </>
  );
}

const fmtW = (w?: number | null) => (w == null ? '—' : Math.round(w) + ' W');
const KIND_LABEL: Record<string, string> = { temperature: 'Temperatures', fan: 'Fans', power: 'Power supplies', voltage: 'Voltages', current: 'Currents', other: 'Other sensors' };
const tempTone = (st: string) => (st === 'red' ? 'text-crit' : st === 'yellow' ? 'text-warn' : '');

function Hosts({ hosts, connId }: { hosts: any[]; connId: string }) {
  const [open, setOpen] = useState<string | null>(null);
  if (!hosts.length) return <Card><Empty icon="server" title="No hosts" /></Card>;
  const anyHw = hosts.some((h) => h.temp_c != null || h.fans > 0);
  return (
    <Card pad={false}>
      <div class="table-wrap">
        <table class="table">
          <thead><tr><th>Host</th><th>State</th><th class="hide-sm">CPU</th><th class="hide-sm">Memory</th><th>VMs</th><th>Power</th>{anyHw && <th class="hide-sm">Temp / fans</th>}<th class="hide-md">Hardware</th><th class="hide-md">ESXi</th><th class="hide-sm">Uptime</th></tr></thead>
          <tbody>
            {hosts.map((h) => (
              <Fragment key={h.ref}>
                <tr>
                  <td>
                    <div class="dev-cell">
                      <Badge tone={statusTone(h.status)}>{h.status === 'gray' ? 'n/a' : h.status}</Badge>
                      <div>
                        <a href={h.ui_url} target="_blank" rel="noopener noreferrer" class="strong">{h.name} <Icon name="external" size={12} /></a>
                        <div class="muted small">{h.cluster || 'standalone'}</div>
                        {h.sensors?.map((s: any) => <div key={s.name} class={'small ' + (s.state === 'red' ? 'text-crit' : 'text-warn')}><Icon name="alert" size={12} /> {s.name} {s.state}{s.reading ? ` (${s.reading})` : ''}</div>)}
                        {(h.readings?.length > 0 || h.power_watts != null || h.power_note) && <button class="linklike small" onClick={() => setOpen(open === h.ref ? null : h.ref)}>{open === h.ref ? 'hide hardware details' : 'hardware details'}</button>}
                      </div>
                    </div>
                  </td>
                  <td>{hostState(h)}</td>
                  <td class="hide-sm">{h.connection_state === 'connected' ? <Meter value={h.cpu_pct} /> : <span class="muted">—</span>}<div class="muted small">{h.cpu_cores} cores · {(h.cpu_mhz / 1000).toFixed(1)} GHz</div></td>
                  <td class="hide-sm">{h.connection_state === 'connected' ? <Meter value={h.mem_pct} /> : <span class="muted">—</span>}<div class="muted small">{fmtBytes(h.mem_total, 0)}</div></td>
                  <td>{h.vms_on}<span class="muted"> / {h.vms}</span></td>
                  <td class="nowrap">
                    {h.power_watts != null
                      ? <>{fmtW(h.power_watts)}<div class="muted small" title={h.power_source === 'estimate' ? 'No measurement: ' + (h.power_note || '') : ''}>{h.power_source === 'sensor' ? 'power sensor' : h.power_source === 'estimate' ? <span class="linklike-plain">estimated from CPU</span> : 'vSphere counter'}{h.power_cap_watts != null && ` · cap ${Math.round(h.power_cap_watts)} W`}</div></>
                      : <span class="muted" title={h.power_note || ''}>—<div class="small">{h.connection_state === 'connected' ? <button class="linklike small" onClick={() => setOpen(h.ref)}>not reported</button> : 'offline'}</div></span>}
                  </td>
                  {anyHw && <td class="hide-sm nowrap">{h.temp_c != null ? `${Math.round(h.temp_c)} °C` : '—'}<div class="muted small">{h.fans > 0 ? `${h.fans} fans · min ${Math.round(h.fan_min_rpm)} RPM` : 'no fan data'}</div></td>}
                  <td class="hide-md small">{h.vendor} {h.model}<div class="muted truncate" title={h.cpu_model}>{h.cpu_model}</div></td>
                  <td class="hide-md small">{h.version}<div class="muted">build {h.build}</div></td>
                  <td class="hide-sm nowrap">{fmtUp(h.uptime_sec)}</td>
                </tr>
                {open === h.ref && <tr><td colSpan={12}><HostHardware host={h} connId={connId} /></td></tr>}
              </Fragment>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

/** Sensor readings and power / temperature history for one host. */
function HostHardware({ host, connId }: { host: any; connId: string }) {
  const [hours, setHours] = useState(24);
  const { data } = useFetch<any[]>(`/vsphere/connections/${connId}/hosts/${host.ref}/power?hours=${hours}`, [host.ref, hours]);
  const pts = data || [];
  const groups: Record<string, any[]> = {};
  for (const r of host.readings || []) (groups[r.kind] ||= []).push(r);
  return (
    <div class="pad">
      <div class="toolbar">
        <strong class="grow">{host.name}: power, temperature and fans</strong>
        {[[1, '1h'], [24, '24h'], [168, '7d'], [720, '30d']].map(([h, l]) => <Button key={h} size="sm" variant={hours === h ? 'primary' : 'default'} onClick={() => setHours(h as number)}>{l as string}</Button>)}
      </div>
      {host.power_note && (host.power_watts == null || host.power_source === 'estimate') && <p class="callout warn small"><Icon name="info" size={14} /><span>No power measurement{host.power_source === 'estimate' ? `, so the ${Math.round(host.power_watts)} W shown is estimated from CPU load` : ''}: {host.power_note}.</span></p>}
      <div class="grid-2">
        {host.power_watts != null || pts.some((p) => p.watts != null) ? <Card title="Power draw (W)"><LineChart data={pts} series={[{ key: 'watts', label: 'Watts', color: 'var(--series-1)' }]} format={(v) => Math.round(v) + ' W'} empty="History builds up as the host is polled" /></Card> : null}
        {pts.some((p) => p.temp_c != null) && <Card title="Hottest sensor (°C)"><LineChart data={pts} series={[{ key: 'temp_c', label: '°C', color: 'var(--series-2)' }]} format={(v) => v.toFixed(0) + ' °C'} /></Card>}
        {pts.some((p) => p.fan_min_rpm != null) && <Card title="Slowest fan (RPM)"><LineChart data={pts} series={[{ key: 'fan_min_rpm', label: 'RPM', color: 'var(--series-1)' }]} format={(v) => Math.round(v) + ' RPM'} /></Card>}
      </div>
      {Object.keys(groups).length === 0
        ? <p class="muted small">This host reports no hardware sensors through vSphere{host.power_watts == null ? ' and no power counter' : ''}. Many servers need the vendor's ESXi image or CIM provider for sensor data; some only expose power draw.</p>
        : Object.entries(groups).map(([kind, rs]) => (
          <div key={kind} class="mb">
            <div class="muted small">{KIND_LABEL[kind] || kind}</div>
            <div class="chips">{rs.map((r: any) => <span key={r.name} class={'chip ' + tempTone(r.state)} style="cursor:default" title={r.state}>{r.name}: <strong>{r.value} {r.unit}</strong></span>)}</div>
          </div>
        ))}
    </div>
  );
}

function Datastores({ dss, conn }: { dss: any[]; conn: any }) {
  if (!dss.length) return <Card><Empty icon="disk" title="No datastores" /></Card>;
  return (
    <Card pad={false}>
      <div class="table-wrap">
        <table class="table">
          <thead><tr><th>Datastore</th><th>Type</th><th>Used</th><th>Free</th><th class="hide-sm">Capacity</th><th>State</th></tr></thead>
          <tbody>
            {dss.map((d) => (
              <tr key={d.ref}>
                <td><span class="strong">{d.name}</span></td>
                <td class="muted">{d.type}</td>
                <td style="min-width:140px">{d.capacity > 0 ? <Meter value={d.used_pct} warn={conn.ds_warn_pct} crit={conn.ds_crit_pct} /> : <span class="muted">—</span>}</td>
                <td>{fmtBytes(d.free)}</td>
                <td class="hide-sm">{fmtBytes(d.capacity)}</td>
                <td>{!d.accessible ? <Badge tone="critical">inaccessible</Badge> : d.maintenance !== 'normal' ? <Badge tone="warning">{d.maintenance}</Badge> : <Badge tone="good">accessible</Badge>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

function Alarms({ alarms }: { alarms: any[] }) {
  if (!alarms.length) return <Card><Empty icon="check" title="No triggered alarms">vCenter reports nothing wrong.</Empty></Card>;
  return (
    <Card pad={false}>
      <div class="table-wrap">
        <table class="table">
          <thead><tr><th>Status</th><th>Alarm</th><th>Object</th><th>Since</th></tr></thead>
          <tbody>
            {alarms.map((a) => (
              <tr key={a.entity_ref + a.alarm_ref}>
                <td><Badge tone={statusTone(a.status)}>{a.status === 'red' ? 'critical' : 'warning'}</Badge>{a.acknowledged && <div class="muted small">acknowledged</div>}</td>
                <td class="strong">{a.alarm_name}</td>
                <td>{a.entity_name}<div class="muted small">{a.entity_type}</div></td>
                <td class="nowrap muted" title={fmtDate(a.time)}>{timeAgo(a.time)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

// ---------- virtual machines ----------

function VMs({ conn, vms, tech, admin, reload, refresh, busy }: { conn: any; vms: any[]; tech: boolean; admin: boolean; reload: () => void; refresh: () => void; busy: boolean }) {
  const [search, setSearch] = useState('');
  const [filter, setFilter] = useState('');
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [actions, setActions] = useState<any | null>(null);
  const [deploy, setDeploy] = useState(false);

  const rows = useMemo(() => {
    const s = search.trim().toLowerCase();
    return vms.filter((v) => {
      if (v.template && filter !== 'templates') return false;
      if (filter === 'on' && v.power_state !== 'poweredOn') return false;
      if (filter === 'off' && v.power_state === 'poweredOn') return false;
      if (filter === 'noagent' && (v.agent || deployBlock(v))) return false;
      if (filter === 'snapshots' && !v.snapshots) return false;
      if (filter === 'templates' && !v.template) return false;
      if (!s) return true;
      return [v.name, v.hostname, v.guest_os, v.host_name, ...(v.ips || [])].some((x) => (x || '').toLowerCase().includes(s));
    });
  }, [vms, search, filter]);

  const toggle = (ref: string) => { const n = new Set(selected); n.has(ref) ? n.delete(ref) : n.add(ref); setSelected(n); };
  const selectable = rows.filter((v) => !deployBlock(v));
  const allSel = selectable.length > 0 && selectable.every((v) => selected.has(v.ref));
  const chosen = vms.filter((v) => selected.has(v.ref));

  return (
    <>
      <div class="toolbar">
        <SearchInput value={search} onInput={setSearch} placeholder="Search name, IP, host, OS…" />
        <select value={filter} onChange={(e) => setFilter((e.target as HTMLSelectElement).value)}>
          <option value="">All VMs</option>
          <option value="on">Powered on</option>
          <option value="off">Powered off</option>
          <option value="noagent">Can receive the agent</option>
          <option value="snapshots">With snapshots</option>
          <option value="templates">Templates</option>
        </select>
        <Button icon="refresh" variant="ghost" onClick={refresh} loading={busy} title="Refresh now" />
        <div class="grow" />
        {admin && <Button icon="download" variant="primary" disabled={selected.size === 0} onClick={() => setDeploy(true)}>Install agent on {selected.size || ''} VM{selected.size === 1 ? '' : 's'}</Button>}
      </div>
      <Card pad={false}>
        {rows.length === 0 ? <Empty icon="box" title="No VMs match" /> : (
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  {admin && <th class="w-check"><input type="checkbox" checked={allSel} onChange={() => setSelected(allSel ? new Set() : new Set(selectable.map((v) => v.ref)))} aria-label="Select all deployable VMs" /></th>}
                  <th>Virtual machine</th><th>Power</th><th class="hide-sm">Guest OS</th><th class="hide-md">Host</th><th class="hide-sm">Resources</th><th>Apex agent</th><th />
                </tr>
              </thead>
              <tbody>
                {rows.map((v) => {
                  const block = deployBlock(v);
                  return (
                    <tr key={v.ref} class={selected.has(v.ref) ? 'selected' : ''}>
                      {admin && <td class="w-check"><input type="checkbox" disabled={!!block} title={block} checked={selected.has(v.ref)} onChange={() => toggle(v.ref)} aria-label={'Select ' + v.name} /></td>}
                      <td>
                        <span class="strong">{v.name}</span> {v.template && <Badge>template</Badge>}
                        <div class="muted small">{v.ips?.[0] || v.hostname || '—'}{v.snapshots > 0 && <> · <span class="text-warn" title={v.oldest_snapshot ? 'oldest ' + fmtDate(v.oldest_snapshot) : ''}>{v.snapshots} snapshot{v.snapshots > 1 ? 's' : ''}</span></>}</div>
                      </td>
                      <td><Badge tone={powerTone(v.power_state)}>{powerLabel(v.power_state)}</Badge></td>
                      <td class="hide-sm small">{osName(v.os_family)}<div class="muted truncate" style="max-width:200px" title={v.guest_os}>{v.guest_os}</div></td>
                      <td class="hide-md small">{v.host_name || '—'}</td>
                      <td class="hide-sm small">{v.cpus} vCPU · {fmtMB(v.mem_mb)}<div class="muted">{fmtBytes(v.disk_used, 0)} used</div></td>
                      <td>
                        {v.agent
                          ? <Link href={'/devices/' + v.agent.device_id} class="strong"><Badge tone={v.agent.online ? 'good' : 'warning'}>{v.agent.online ? 'online' : 'offline'}</Badge></Link>
                          : block ? <span class="muted small" title={block}>—</span> : <span class="muted small">not installed</span>}
                      </td>
                      <td class="right nowrap">
                        {tech && !v.template && <Button size="sm" variant="ghost" icon="power" onClick={() => setActions(v)}>Power</Button>}
                        <a class="btn btn-ghost btn-sm" href={v.ui_url} target="_blank" rel="noopener noreferrer" title="Open in the vSphere client (console available there)"><Icon name="external" size={15} /></a>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>
      {actions && <PowerModal conn={conn} vm={actions} onClose={() => setActions(null)} onDone={() => { setActions(null); setTimeout(reload, 4000); }} />}
      {deploy && <DeployModal conn={conn} vms={chosen} onClose={() => setDeploy(false)} onStarted={(batch) => { setDeploy(false); setSelected(new Set()); navigate(`/virtualization?c=${conn.id}&tab=deploys&batch=${batch}`); }} />}
    </>
  );
}

const POWER: { id: string; label: string; icon: string; needs: 'off' | 'on' | 'tools'; danger?: boolean; hint: string }[] = [
  { id: 'on', label: 'Power on', icon: 'play', needs: 'off', hint: 'Starts the VM.' },
  { id: 'shutdown', label: 'Shut down guest', icon: 'power', needs: 'tools', hint: 'Clean shutdown through VMware Tools.' },
  { id: 'reboot', label: 'Restart guest', icon: 'refresh', needs: 'tools', hint: 'Clean restart through VMware Tools.' },
  { id: 'suspend', label: 'Suspend', icon: 'clock', needs: 'on', hint: 'Saves the VM state to disk and stops it.' },
  { id: 'reset', label: 'Reset (hard)', icon: 'refresh', needs: 'on', danger: true, hint: 'Like pressing the reset button: no clean shutdown, unsaved data is lost.' },
  { id: 'off', label: 'Power off (hard)', icon: 'power', needs: 'on', danger: true, hint: 'Like pulling the plug: no clean shutdown, unsaved data is lost.' },
];

function PowerModal({ conn, vm, onClose, onDone }: { conn: any; vm: any; onClose: () => void; onDone: () => void }) {
  const [busy, setBusy] = useState('');
  const toolsOk = vm.tools_running === 'guestToolsRunning';
  const run = async (a: (typeof POWER)[number]) => {
    if (!(await confirmDialog({ title: `${a.label}: ${vm.name}?`, body: a.hint, confirm: a.label, danger: !!a.danger }))) return;
    setBusy(a.id);
    if (await attempt(() => api.post(`/vsphere/connections/${conn.id}/vms/${vm.ref}/power`, { action: a.id }), `${a.label} sent to ${vm.name}`)) onDone();
    setBusy('');
  };
  const enabled = (a: (typeof POWER)[number]) =>
    a.needs === 'off' ? vm.power_state !== 'poweredOn' : vm.power_state === 'poweredOn' && (a.needs !== 'tools' || toolsOk);
  return (
    <Modal title={<>Power: {vm.name}</>} onClose={onClose} width={420}>
      <div class="menu" style="position:static;box-shadow:none;border:0;padding:0;min-width:0">
        {POWER.map((a) => (
          <button key={a.id} class={a.danger ? 'danger' : ''} disabled={!enabled(a) || !!busy} onClick={() => run(a)} title={!enabled(a) && a.needs === 'tools' && vm.power_state === 'poweredOn' ? 'VMware Tools is not running' : a.hint}>
            {busy === a.id ? <span class="spinner sm" /> : <Icon name={a.icon} size={16} />} {a.label}
          </button>
        ))}
      </div>
      <p class="muted small">Currently {powerLabel(vm.power_state).toLowerCase()}{vm.power_state === 'poweredOn' && !toolsOk ? ' · VMware Tools is not running, so only the hard actions are available' : ''}. Actions are recorded in the audit log.</p>
    </Modal>
  );
}

// ---------- deploy agent ----------

function DeployModal({ conn, vms, onClose, onStarted }: { conn: any; vms: any[]; onClose: () => void; onStarted: (batch: string) => void }) {
  const clients = useFetch<any[]>('/clients');
  const [clientId, setClientId] = useState<string>(conn.client_id || '');
  const [siteId, setSiteId] = useState('');
  const [win, setWin] = useState({ username: 'Administrator', password: '' });
  const [lin, setLin] = useState({ username: 'root', password: '' });
  const [force, setForce] = useState(false);
  const [busy, setBusy] = useState(false);
  useEffect(() => { if (!clientId && clients.data?.length) setClientId(clients.data[0].id); }, [clients.data]);

  const withAgent = vms.filter((v) => v.agent);
  const targets = vms.filter((v) => force || !v.agent);
  const nWin = targets.filter((v) => v.os_family === 'windows').length;
  const nLin = targets.filter((v) => v.os_family === 'linux').length;
  const sites: any[] = clients.data?.find((c) => c.id === clientId)?.sites || [];
  const ready = targets.length > 0 && clientId && (!nWin || (win.username && win.password)) && (!nLin || (lin.username && lin.password));

  const start = async () => {
    setBusy(true);
    try {
      const res = await api.post(`/vsphere/connections/${conn.id}/deploy`, {
        vms: vms.map((v) => v.ref), client_id: clientId, site_id: siteId || null, force,
        windows: nWin ? win : null, linux: nLin ? lin : null,
      });
      toast('Installing the agent… this takes a minute or two per VM.', 'info');
      onStarted(res.batch_id);
    } catch (e: any) { toast(e.message, 'error'); }
    setBusy(false);
  };
  return (
    <Modal title="Install the Apex agent into VMs" onClose={onClose} width={620}
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="primary" icon="download" disabled={!ready} loading={busy} onClick={start}>Install on {targets.length} VM{targets.length === 1 ? '' : 's'}</Button></>}>
      <p class="muted">Runs the installer inside each VM through VMware Tools. The VMs must be able to reach this server at its public address to download the agent. The guest accounts below are used once for this run and are <strong>not stored</strong>.</p>
      <div class="row-fields">
        <Field label="New devices belong to client">
          <select value={clientId} onChange={(e) => { setClientId((e.target as HTMLSelectElement).value); setSiteId(''); }}>
            {clients.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
          </select>
        </Field>
        <Field label="Site (optional)">
          <select value={siteId} onChange={(e) => setSiteId((e.target as HTMLSelectElement).value)}>
            <option value="">—</option>
            {sites.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
          </select>
        </Field>
      </div>
      {nWin > 0 && (
        <Card title={`Windows account (${nWin} VM${nWin > 1 ? 's' : ''})`}>
          <div class="row-fields">
            <Field label="Username" hint="Local administrator: .\Administrator. Domain: DOMAIN\user"><input value={win.username} onInput={(e) => setWin({ ...win, username: (e.target as HTMLInputElement).value })} autoComplete="off" /></Field>
            <Field label="Password"><input type="password" value={win.password} autoComplete="new-password" onInput={(e) => setWin({ ...win, password: (e.target as HTMLInputElement).value })} /></Field>
          </div>
        </Card>
      )}
      {nLin > 0 && (
        <Card title={`Linux account (${nLin} VM${nLin > 1 ? 's' : ''})`}>
          <div class="row-fields">
            <Field label="Username" hint="root, or a user that can use sudo (its password is also given to sudo)"><input value={lin.username} onInput={(e) => setLin({ ...lin, username: (e.target as HTMLInputElement).value })} autoComplete="off" /></Field>
            <Field label="Password"><input type="password" value={lin.password} autoComplete="new-password" onInput={(e) => setLin({ ...lin, password: (e.target as HTMLInputElement).value })} /></Field>
          </div>
        </Card>
      )}
      {withAgent.length > 0 && (
        <label class="check"><input type="checkbox" checked={force} onChange={() => setForce(!force)} /> Also reinstall on {withAgent.length} VM{withAgent.length > 1 ? 's' : ''} that already {withAgent.length > 1 ? 'have' : 'has'} an agent</label>
      )}
      <details>
        <summary>VMs ({vms.length})</summary>
        <ul class="list">{vms.map((v) => <li key={v.ref}><span class="strong">{v.name}</span><span class="muted small">{osName(v.os_family)}{v.agent ? (force ? ' · will reinstall' : ' · skipped, agent present') : ''}</span></li>)}</ul>
      </details>
      <p class="muted small">vCenter account needs the “Guest operations” privileges (modifications, program execution, queries) on these VMs.</p>
    </Modal>
  );
}

// ---------- deployments ----------

function Deploys({ conn }: { conn: any }) {
  const q = useQuery();
  const batch = q.get('batch') || '';
  const { data, reload } = useFetch<any[]>(`/vsphere/connections/${conn.id}/deploys`, [], 4000);
  const [open, setOpen] = useState<string>('');
  useEffect(() => { reload(true); }, [batch]);
  if (!data) return <Spinner />;
  if (!data.length) return <Card><Empty icon="download" title="No deployments yet">Select VMs on the Virtual machines tab and choose “Install agent”.</Empty></Card>;
  const tone: Record<string, string> = { success: 'good', failed: 'critical', running: 'accent', queued: 'neutral', skipped: 'neutral' };
  return (
    <Card pad={false}>
      <div class="table-wrap">
        <table class="table">
          <thead><tr><th>VM</th><th>Status</th><th>Result</th><th class="hide-sm">Started by</th><th>When</th></tr></thead>
          <tbody>
            {data.map((d) => (
              <Fragment key={d.id}>
                <tr class={d.batch_id === batch ? 'selected' : ''}>
                  <td><span class="strong">{d.vm_name}</span><div class="muted small">{osName(d.os_family)}</div></td>
                  <td><Badge tone={tone[d.status] || 'neutral'}>{d.status === 'running' ? 'installing…' : d.status}</Badge></td>
                  <td class="small">{d.message}{d.log && <> · <button class="linklike" onClick={() => setOpen(open === d.id ? '' : d.id)}>{open === d.id ? 'hide log' : 'installer log'}</button></>}</td>
                  <td class="hide-sm small">{d.created_by}</td>
                  <td class="nowrap muted" title={fmtDate(d.created_at)}>{timeAgo(d.created_at)}</td>
                </tr>
                {open === d.id && <tr><td colSpan={5}><pre class="output">{d.log}</pre></td></tr>}
              </Fragment>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

// ---------- connections (admin) ----------

function Connections({ conns, reload }: { conns: any[]; reload: () => void }) {
  const [edit, setEdit] = useState<any | null>(null);
  return (
    <>
      <div class="toolbar"><p class="muted grow">Apex RMM reads vCenter / ESXi through the vSphere API: nothing is installed on the hosts. Use a dedicated account (read-only is enough for monitoring; add power and guest-operation privileges to use those features).</p><Button variant="primary" icon="plus" onClick={() => setEdit({})}>Add connection</Button></div>
      <Card pad={false}>
        <table class="table">
          <thead><tr><th>Connection</th><th>Status</th><th class="hide-sm">Client</th><th class="hide-sm">Last read</th><th /></tr></thead>
          <tbody>
            {conns.map((c) => (
              <tr key={c.id}>
                <td><button class="linklike strong" onClick={() => setEdit(c)}>{c.name}</button><div class="muted small">{c.url} · {c.username}</div></td>
                <td>{!c.enabled ? <Badge>disabled</Badge> : c.status === 'ok' ? <Badge tone="good">connected</Badge> : c.status === 'error' ? <Badge tone="critical">error</Badge> : <Badge>pending</Badge>}
                  {c.status === 'error' && c.last_error && <div class="small text-crit" style="max-width:420px">{c.last_error}</div>}
                  {c.about?.full_name && <div class="muted small">{c.about.full_name}</div>}</td>
                <td class="hide-sm">{c.client_name || <span class="muted">—</span>}</td>
                <td class="hide-sm muted">{timeAgo(c.last_poll)}</td>
                <td class="right"><Button size="sm" variant="ghost" icon="edit" onClick={() => setEdit(c)}>Edit</Button></td>
              </tr>
            ))}
          </tbody>
        </table>
      </Card>
      {edit && <ConnectionEditor conn={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload(); }} />}
    </>
  );
}

const DEFAULTS = { enabled: true, insecure: false, poll_seconds: 60, ds_warn_pct: 85, ds_crit_pct: 95, snapshot_days: 3, alert_alarms: true, alert_hosts: true, create_tickets: false, notify: true };

function ConnectionEditor({ conn, onClose, onSaved }: { conn: any; onClose: () => void; onSaved: () => void }) {
  const clients = useFetch<any[]>('/clients');
  const [v, setV] = useState<any>({ ...DEFAULTS, ...conn, password: '' });
  const [test, setTest] = useState<any>(null);
  const [testing, setTesting] = useState(false);
  const [busy, setBusy] = useState(false);
  const set = (k: string, val: any) => setV({ ...v, [k]: val });
  const txt = (k: string, props: any = {}) => <input value={v[k] ?? ''} onInput={(e) => set(k, (e.target as HTMLInputElement).value)} {...props} />;
  const num = (k: string, props: any = {}) => <input type="number" value={v[k]} onInput={(e) => set(k, +(e.target as HTMLInputElement).value)} {...props} />;
  const chk = (k: string, label: string) => <label class="check"><input type="checkbox" checked={!!v[k]} onChange={() => set(k, !v[k])} /> {label}</label>;

  const runTest = async () => {
    setTesting(true); setTest(null);
    try { setTest(await api.post('/vsphere/test', { id: v.id, url: v.url, username: v.username, password: v.password, insecure: v.insecure, thumbprint: v.thumbprint })); }
    catch (e: any) { setTest({ ok: false, error: e.message }); }
    setTesting(false);
  };
  const save = async () => {
    setBusy(true);
    const body = { ...v, client_id: v.client_id || null };
    if (await attempt(() => (v.id ? api.put('/vsphere/connections/' + v.id, body) : api.post('/vsphere/connections', body)), 'Connection saved')) onSaved();
    setBusy(false);
  };
  const del = async () => {
    if (!(await confirmDialog({ title: `Delete "${v.name}"?`, body: 'Its inventory and open alerts are removed. Agents already installed in VMs keep working.', danger: true, confirm: 'Delete' }))) return;
    if (await attempt(() => api.del('/vsphere/connections/' + v.id), 'Connection deleted')) onSaved();
  };
  return (
    <Modal title={v.id ? 'Edit vSphere connection' : 'Add vSphere connection'} onClose={onClose} width={680}
      footer={<>{v.id && <Button variant="danger" icon="trash" onClick={del}>Delete</Button>}<div class="grow" /><Button onClick={onClose}>Cancel</Button><Button variant="primary" loading={busy} disabled={!v.name || !v.url || !v.username || (!v.id && !v.password)} onClick={save}>Save</Button></>}>
      <div class="row-fields">
        <Field label="Name">{txt('name', { placeholder: 'Main vCenter' })}</Field>
        <Field label="Client" hint="New devices from this connection belong here."><select value={v.client_id || ''} onChange={(e) => set('client_id', (e.target as HTMLSelectElement).value)}><option value="">—</option>{clients.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
      </div>
      <Field label="vCenter / ESXi address" hint="Host name or IP, e.g. vcenter.example.com. The server must be able to reach it on port 443.">{txt('url', { placeholder: 'vcenter.example.com' })}</Field>
      <div class="row-fields">
        <Field label="Username">{txt('username', { placeholder: 'apex@vsphere.local', autoComplete: 'off' })}</Field>
        <Field label="Password" hint={v.password_set && !v.password ? 'A password is saved. Leave empty to keep it.' : 'Stored encrypted.'}>
          <input type="password" autoComplete="new-password" value={v.password} placeholder={v.password_set ? '••••••••••••' : ''} onInput={(e) => set('password', (e.target as HTMLInputElement).value)} />
        </Field>
      </div>
      <div class="inline" style="margin:0 0 8px">
        <Button onClick={runTest} loading={testing} disabled={!v.url || !v.username || (!v.password && !v.id)}>Test connection</Button>
        {chk('insecure', 'Do not verify the TLS certificate')}
      </div>
      {test && (test.ok
        ? <div class="small text-good mb">Connected ✓ {test.about.full_name} — {test.hosts} host{test.hosts === 1 ? '' : 's'}, {test.vms} VM{test.vms === 1 ? '' : 's'}, {test.datastores} datastore{test.datastores === 1 ? '' : 's'}</div>
        : test.cert_untrusted
          ? <div class="errorbox small">
              This server's certificate is not signed by a trusted authority (normal for a default vCenter/ESXi certificate).
              {test.thumbprint && <><br />Fingerprint: <span class="mono">{test.thumbprint}</span><br /><button class="chip" onClick={() => { set('thumbprint', test.thumbprint); setTest(null); }}>Trust exactly this certificate</button></>}
            </div>
          : <div class="errorbox small">{test.error}</div>)}
      {v.thumbprint && <Field label="Trusted certificate fingerprint" hint="Only a certificate with this fingerprint is accepted. Clear it to require a publicly trusted certificate.">{txt('thumbprint', { class: 'mono' })}</Field>}

      <details open={!v.id}>
        <summary>Alerts and polling</summary>
        <div class="row-fields">
          <Field label="Read every (seconds)">{num('poll_seconds', { min: 30, max: 3600 })}</Field>
          <Field label="Datastore warning (% full)">{num('ds_warn_pct', { min: 1, max: 100 })}</Field>
          <Field label="Datastore critical (% full)">{num('ds_crit_pct', { min: 1, max: 100 })}</Field>
          <Field label="Old snapshot (days, 0 = off)">{num('snapshot_days', { min: 0, max: 365 })}</Field>
        </div>
        <div class="checks col">
          {chk('alert_hosts', 'Alert when a host stops responding or reports a hardware problem')}
          {chk('alert_alarms', 'Raise triggered vCenter alarms as alerts')}
          {chk('notify', 'Send the webhook notification configured in Settings')}
          {chk('create_tickets', 'Automatically open a ticket for new alerts')}
          {chk('enabled', 'Connection enabled')}
        </div>
      </details>
    </Modal>
  );
}
