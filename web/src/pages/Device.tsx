import { useMemo, useState } from 'preact/hooks';
import { api } from '../api';
import { LineChart } from '../chart';
import { Icon } from '../icons';
import { Link, navigate, useQuery } from '../router';
import {
  Badge, Button, Card, CopyText, Empty, ErrorBox, Field, Kv, Meter, Modal, OsBadge, PageHeader, Priority, SearchInput, Severity, Spinner,
  StatusBadge, StatusDot, Tabs, attempt, confirmDialog, deviceName, fmtBytes, fmtDate, fmtUptime, timeAgo, toast, useFetch,
} from '../ui';
import { useCan } from '../user';
import { JobOutput, RunScriptModal } from './Scripts';
import { TerminalView } from './Terminal';
import { NewTicketModal } from './Tickets';

const SERIES_BLUE = 'var(--series-1)';
const SERIES_ORANGE = 'var(--series-2)';

export function DevicePage({ id }: { id: string }) {
  const q = useQuery();
  const tab = q.get('tab') || 'overview';
  const setTab = (t: string) => navigate(`/devices/${id}?tab=${t}`, true);
  const { data: d, error, reload, setData } = useFetch<any>('/devices/' + id, [], 15000);
  const tech = useCan('technician');
  const admin = useCan('admin');
  const [runOpen, setRunOpen] = useState(false);
  const [rdOpen, setRdOpen] = useState(false);
  const [menu, setMenu] = useState(false);
  const [ticketOpen, setTicketOpen] = useState(false);

  if (error) return <div class="page"><ErrorBox msg={error} /></div>;
  if (!d) return <div class="page"><Spinner /></div>;

  const openRemote = () => {
    window.open(`/devices/${id}/remote`, 'hardy-rd-' + id, 'popup,width=1400,height=900');
  };
  const power = async (action: string) => {
    if (!(await confirmDialog({ title: `${action === 'reboot' ? 'Restart' : 'Shut down'} ${deviceName(d)}?`, body: 'Unsaved work on the device may be lost.', confirm: action === 'reboot' ? 'Restart' : 'Shut down', danger: true }))) return;
    attempt(() => api.post(`/devices/${id}/power`, { action }), `${action === 'reboot' ? 'Restart' : 'Shutdown'} command sent`);
  };
  const refresh = () => attempt(async () => setData(await api.post(`/devices/${id}/refresh`)), 'Inventory refreshed');
  const updateAgent = () => attempt(() => api.post(`/devices/${id}/update-agent`), 'Agent update started — it will reconnect shortly');
  const del = async () => {
    if (!(await confirmDialog({ title: `Remove ${deviceName(d)}?`, body: 'The agent will be uninstalled if the device is online, and all history for it is deleted.', confirm: 'Remove device', danger: true }))) return;
    if (await attempt(() => api.del('/devices/' + id), 'Device removed')) navigate('/devices');
  };
  const inMaint = d.maintenance_until && new Date(d.maintenance_until) > new Date();
  const maint = async () => {
    const body = inMaint ? { clear_maintenance: true } : { maintenance_until: new Date(Date.now() + 4 * 3600e3).toISOString() };
    attempt(async () => setData(await api.patch('/devices/' + id, body)), inMaint ? 'Maintenance mode ended' : 'Alerts suppressed for 4 hours');
  };

  const tabs = [
    { id: 'overview', label: 'Overview', icon: 'info' },
    { id: 'performance', label: 'Performance', icon: 'activity' },
    ...(tech ? [{ id: 'terminal', label: 'Terminal', icon: 'terminal' }, { id: 'processes', label: 'Processes', icon: 'list' }, { id: 'services', label: 'Services', icon: 'server' }, { id: 'software', label: 'Software', icon: 'box' }] : []),
    { id: 'scripts', label: 'History', icon: 'code' },
    { id: 'alerts', label: 'Alerts', icon: 'bell', count: d.open_alerts },
    { id: 'tickets', label: 'Tickets', icon: 'ticket' },
    ...(tech ? [{ id: 'settings', label: 'Details', icon: 'edit' }] : []),
  ];

  return (
    <div class="page">
      <PageHeader
        back={<Link href="/devices" class="back"><Icon name="chevronLeft" size={16} /> Devices</Link>}
        title={<span class="title-row">{deviceName(d)} <StatusDot online={d.online} /></span>}
        subtitle={<span class="meta-row"><OsBadge os={d.os} /> <span>{d.platform} {d.os_version}</span> <span>·</span> <span>{d.client_name || 'No client'}{d.site_name ? ' / ' + d.site_name : ''}</span> {inMaint && <Badge tone="warning">Maintenance until {new Date(d.maintenance_until).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</Badge>}</span>}
        actions={tech && <>
          <Button variant="primary" icon="pointer" disabled={!d.online} onClick={openRemote} title={d.online ? 'Open built-in remote control' : 'Device is offline'}>Remote control</Button>
          <Button icon="link" onClick={() => setRdOpen(true)}>RustDesk</Button>
          <Button icon="play" disabled={!d.online && false} onClick={() => setRunOpen(true)}>Run script</Button>
          <div class="menu-wrap">
            <Button icon="more" onClick={() => setMenu(!menu)} aria-label="More actions" />
            {menu && (
              <div class="menu" onClick={() => setMenu(false)}>
                <button disabled={!d.online} onClick={refresh}><Icon name="refresh" size={16} /> Refresh inventory</button>
                <button onClick={() => setTicketOpen(true)}><Icon name="ticket" size={16} /> Create ticket</button>
                <button onClick={maint}><Icon name="clock" size={16} /> {inMaint ? 'End maintenance mode' : 'Maintenance mode (4h)'}</button>
                <button disabled={!d.online} onClick={updateAgent}><Icon name="download" size={16} /> Update agent</button>
                <hr />
                <button disabled={!d.online} onClick={() => power('reboot')}><Icon name="refresh" size={16} /> Restart</button>
                <button disabled={!d.online} onClick={() => power('shutdown')}><Icon name="power" size={16} /> Shut down</button>
                {admin && <><hr /><button class="danger" onClick={del}><Icon name="trash" size={16} /> Remove device</button></>}
              </div>
            )}
          </div>
        </>}
      />
      <Tabs tabs={tabs} active={tab} onChange={setTab} />
      {tab === 'overview' && <Overview d={d} />}
      {tab === 'performance' && <Performance id={id} />}
      {tab === 'terminal' && (d.online ? <Card pad={false} class="term-card"><TerminalView id={id} os={d.os} /></Card> : <Card><Empty icon="terminal" title="Device is offline">The terminal is available when the agent is connected.</Empty></Card>)}
      {tab === 'processes' && <Processes id={id} online={d.online} />}
      {tab === 'services' && <Services id={id} online={d.online} />}
      {tab === 'software' && <Software id={id} online={d.online} />}
      {tab === 'scripts' && <ScriptHistory id={id} />}
      {tab === 'alerts' && <DeviceAlerts id={id} />}
      {tab === 'tickets' && <DeviceTickets id={id} onNew={() => setTicketOpen(true)} />}
      {tab === 'settings' && <DeviceSettings d={d} onSaved={(n) => { setData(n); toast('Saved'); }} />}
      {runOpen && <RunScriptModal deviceIds={[id]} os={d.os} onClose={() => setRunOpen(false)} onDone={() => setTab('scripts')} />}
      {rdOpen && <RustDeskModal d={d} onClose={() => { setRdOpen(false); reload(true); }} />}
      {ticketOpen && <NewTicketModal deviceId={id} clientId={d.client_id} onClose={() => setTicketOpen(false)} />}
    </div>
  );
}

function Overview({ d }: { d: any }) {
  const inv = d.inventory || {};
  return (
    <div class="grid-3">
      <Card title="Health">
        <div class="health">
          <div><span class="muted"><Icon name="cpu" size={15} /> CPU</span><Meter value={d.online ? d.cpu_pct : 0} /></div>
          <div><span class="muted"><Icon name="memory" size={15} /> Memory</span><Meter value={d.online ? d.mem_pct : 0} /></div>
          <div><span class="muted"><Icon name="disk" size={15} /> Fullest disk</span><Meter value={d.disk_pct} /></div>
        </div>
        <Kv items={[
          ['Status', d.online ? 'Online' : `Offline — last seen ${timeAgo(d.last_seen)}`],
          ['Uptime', d.online ? fmtUptime(d.boot_time) : '—'],
          ['Logged-in users', d.logged_in_users?.join(', ') || 'None'],
          ['Agent version', d.agent_version],
        ]} />
      </Card>
      <Card title="System">
        <Kv items={[
          ['Hostname', d.hostname],
          ['Operating system', `${d.platform} ${d.os_version}`],
          ['Kernel', d.kernel_version],
          ['Architecture', d.arch],
          ['Manufacturer', d.manufacturer || '—'],
          ['Model', d.model || '—'],
          ['Serial', d.serial || '—'],
          ['Virtualization', inv.virtual || 'Physical / unknown'],
        ]} />
      </Card>
      <Card title="Hardware">
        <Kv items={[
          ['Processor', d.cpu_model],
          ['Cores / threads', `${inv.cpu_cores || '?'} / ${inv.cpu_threads || d.cpu_cores}`],
          ['Memory', fmtBytes(d.ram_total)],
          ['Enrolled', fmtDate(d.created_at)],
        ]} />
      </Card>
      <Card title="Storage" pad={false}>
        <table class="table compact">
          <thead><tr><th>Volume</th><th>Size</th><th>Used</th></tr></thead>
          <tbody>
            {(inv.disks || []).map((x: any) => (
              <tr key={x.mount}><td class="mono">{x.mount}<div class="muted small">{x.fstype}</div></td><td>{fmtBytes(x.total)}</td><td><Meter value={x.percent} /><div class="muted small">{fmtBytes(x.used)} used</div></td></tr>
            ))}
          </tbody>
        </table>
      </Card>
      <Card title="Network" pad={false} class="span-2">
        <table class="table compact">
          <thead><tr><th>Interface</th><th>MAC</th><th>Addresses</th></tr></thead>
          <tbody>
            <tr><td>Public IP</td><td /><td class="mono">{d.public_ip || '—'}</td></tr>
            {(inv.nics || []).map((n: any) => (
              <tr key={n.name}><td>{n.name}</td><td class="mono small">{n.mac || '—'}</td><td class="mono small">{(n.addrs || []).join(', ')}</td></tr>
            ))}
          </tbody>
        </table>
      </Card>
      {d.notes && <Card title="Notes" class="span-3"><p class="prewrap">{d.notes}</p></Card>}
    </div>
  );
}

function Performance({ id }: { id: string }) {
  const [hours, setHours] = useState(24);
  const { data, loading } = useFetch<any[]>(`/devices/${id}/metrics?hours=${hours}`, [], 60000);
  const pct = (v: number) => v.toFixed(1) + '%';
  const rate = (v: number) => fmtBytes(v) + '/s';
  return (
    <>
      <div class="toolbar">
        <div class="seg">
          {[[1, '1h'], [6, '6h'], [24, '24h'], [168, '7d'], [336, '14d']].map(([h, l]) => (
            <button key={h} class={hours === h ? 'active' : ''} onClick={() => setHours(h as number)}>{l}</button>
          ))}
        </div>
      </div>
      {loading && !data ? <Spinner /> : (
        <div class="grid-2">
          <Card title="CPU usage"><LineChart data={data || []} series={[{ key: 'cpu', label: 'CPU', color: SERIES_BLUE }]} yMax={100} format={pct} /></Card>
          <Card title="Memory usage"><LineChart data={data || []} series={[{ key: 'mem', label: 'Memory', color: SERIES_BLUE }]} yMax={100} format={pct} /></Card>
          <Card title="Disk usage (fullest volume)"><LineChart data={data || []} series={[{ key: 'disk', label: 'Disk', color: SERIES_BLUE }]} yMax={100} format={pct} /></Card>
          <Card title="Network throughput"><LineChart data={data || []} series={[{ key: 'net_rx', label: 'Received', color: SERIES_BLUE }, { key: 'net_tx', label: 'Sent', color: SERIES_ORANGE }]} format={rate} /></Card>
        </div>
      )}
    </>
  );
}

function Processes({ id, online }: { id: string; online: boolean }) {
  const { data, error, loading, reload } = useFetch<any[]>(online ? `/devices/${id}/processes` : null);
  const [s, setS] = useState('');
  const rows = useMemo(() => (data || []).filter((p) => !s || (p.name + ' ' + p.user + ' ' + p.cmdline + ' ' + p.pid).toLowerCase().includes(s.toLowerCase())), [data, s]);
  if (!online) return <Card><Empty icon="list" title="Device is offline" /></Card>;
  const kill = async (p: any) => {
    if (!(await confirmDialog({ title: `End ${p.name} (PID ${p.pid})?`, confirm: 'End process', danger: true }))) return;
    if (await attempt(() => api.post(`/devices/${id}/processes/${p.pid}/kill`), 'Process ended')) reload(true);
  };
  return (
    <Card pad={false} title={`${rows.length} processes`} actions={<><SearchInput value={s} onInput={setS} placeholder="Filter processes" /><Button icon="refresh" variant="ghost" onClick={() => reload()} /></>}>
      {error && <ErrorBox msg={error} />}
      {loading && !data ? <Spinner label="Asking the agent…" /> : (
        <div class="table-wrap tall">
          <table class="table compact">
            <thead><tr><th>Name</th><th>PID</th><th>User</th><th>CPU</th><th>Memory</th><th class="hide-md">Command</th><th /></tr></thead>
            <tbody>
              {rows.slice(0, 500).map((p) => (
                <tr key={p.pid}>
                  <td class="strong">{p.name}</td><td class="mono">{p.pid}</td><td class="truncate">{p.user}</td><td>{p.cpu.toFixed(1)}%</td><td>{fmtBytes(p.mem_rss)}</td>
                  <td class="hide-md mono small truncate" title={p.cmdline}>{p.cmdline}</td>
                  <td><Button size="sm" variant="ghost" icon="x" title="End process" onClick={() => kill(p)} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

function Services({ id, online }: { id: string; online: boolean }) {
  const { data, error, loading, reload } = useFetch<any[]>(online ? `/devices/${id}/services` : null);
  const [s, setS] = useState('');
  const [busy, setBusy] = useState('');
  const rows = useMemo(() => (data || []).filter((x) => !s || (x.name + ' ' + x.display_name).toLowerCase().includes(s.toLowerCase())).sort((a, b) => a.display_name.localeCompare(b.display_name)), [data, s]);
  if (!online) return <Card><Empty icon="server" title="Device is offline" /></Card>;
  const act = async (name: string, action: string) => {
    setBusy(name + action);
    if (await attempt(() => api.post(`/devices/${id}/services/${encodeURIComponent(name)}/${action}`), `${name}: ${action} sent`)) reload(true);
    setBusy('');
  };
  const running = (st: string) => ['running', 'active'].includes(st);
  return (
    <Card pad={false} title={`${rows.length} services`} actions={<><SearchInput value={s} onInput={setS} placeholder="Filter services" /><Button icon="refresh" variant="ghost" onClick={() => reload()} /></>}>
      {error && <ErrorBox msg={error} />}
      {loading && !data ? <Spinner label="Asking the agent…" /> : (
        <div class="table-wrap tall">
          <table class="table compact">
            <thead><tr><th>Service</th><th>Status</th><th class="hide-sm">Startup</th><th /></tr></thead>
            <tbody>
              {rows.map((x) => (
                <tr key={x.name}>
                  <td><span class="strong">{x.display_name || x.name}</span>{x.display_name && x.display_name !== x.name && <div class="muted small mono">{x.name}</div>}</td>
                  <td><Badge tone={running(x.status) ? 'good' : 'neutral'}>{x.status}</Badge></td>
                  <td class="hide-sm">{x.start_type}</td>
                  <td class="nowrap right">
                    {running(x.status)
                      ? <><Button size="sm" variant="ghost" loading={busy === x.name + 'restart'} onClick={() => act(x.name, 'restart')}>Restart</Button><Button size="sm" variant="ghost" loading={busy === x.name + 'stop'} onClick={() => act(x.name, 'stop')}>Stop</Button></>
                      : <Button size="sm" variant="ghost" loading={busy === x.name + 'start'} onClick={() => act(x.name, 'start')}>Start</Button>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

function Software({ id, online }: { id: string; online: boolean }) {
  const { data, error, loading, reload } = useFetch<any[]>(online ? `/devices/${id}/software` : null);
  const [s, setS] = useState('');
  const rows = useMemo(() => (data || []).filter((x) => !s || (x.name + ' ' + x.publisher).toLowerCase().includes(s.toLowerCase())), [data, s]);
  if (!online) return <Card><Empty icon="box" title="Device is offline" /></Card>;
  return (
    <Card pad={false} title={`${rows.length} installed applications`} actions={<><SearchInput value={s} onInput={setS} placeholder="Filter software" /><Button icon="refresh" variant="ghost" onClick={() => reload()} /></>}>
      {error && <ErrorBox msg={error} />}
      {loading && !data ? <Spinner label="Collecting software inventory (can take a minute on macOS)…" /> : (
        <div class="table-wrap tall">
          <table class="table compact">
            <thead><tr><th>Name</th><th>Version</th><th class="hide-sm">Publisher</th></tr></thead>
            <tbody>{rows.map((x, i) => <tr key={i}><td class="strong">{x.name}</td><td class="mono small">{x.version}</td><td class="hide-sm truncate">{x.publisher}</td></tr>)}</tbody>
          </table>
        </div>
      )}
    </Card>
  );
}

function ScriptHistory({ id }: { id: string }) {
  const { data, loading } = useFetch<any[]>(`/devices/${id}/jobs`, [], 5000);
  const [open, setOpen] = useState<string | null>(null);
  if (loading && !data) return <Spinner />;
  if (!data?.length) return <Card><Empty icon="code" title="No scripts have run on this device yet" /></Card>;
  return (
    <Card pad={false}>
      <ul class="list">
        {data.map((r) => (
          <li key={r.id} class="col">
            <div class="row clickable" onClick={() => setOpen(open === r.id ? null : r.id)}>
              <Icon name={open === r.id ? 'chevronDown' : 'chevronRight'} size={16} />
              <StatusBadge s={r.status} />
              <div class="grow"><span class="strong">{r.name}</span> <span class="muted small">{r.shell} · by {r.created_by} · {timeAgo(r.created_at)}</span></div>
              {r.exit_code !== null && <span class="muted small mono">exit {r.exit_code}</span>}
              <Link href={'/jobs/' + r.job_id} class="link small">Job</Link>
            </div>
            {open === r.id && <JobOutput r={r} />}
          </li>
        ))}
      </ul>
    </Card>
  );
}

function DeviceAlerts({ id }: { id: string }) {
  const { data } = useFetch<any[]>(`/devices/${id}/alerts`);
  if (!data) return <Spinner />;
  if (!data.length) return <Card><Empty icon="check" title="No alerts for this device" /></Card>;
  return (
    <Card pad={false}>
      <ul class="list">
        {data.map((a) => (
          <li key={a.id}><Severity s={a.severity} /><div class="grow"><span class="strong">{a.title}</span><div class="muted small">{a.message}</div></div><span class="muted small">{timeAgo(a.created_at)}</span><StatusBadge s={a.status} /></li>
        ))}
      </ul>
    </Card>
  );
}

function DeviceTickets({ id, onNew }: { id: string; onNew: () => void }) {
  const { data } = useFetch<any[]>(`/tickets?device=${id}&status=all`);
  if (!data) return <Spinner />;
  return (
    <Card pad={false} title="Tickets" actions={<Button size="sm" icon="plus" onClick={onNew}>New ticket</Button>}>
      {!data.length ? <Empty icon="ticket" title="No tickets for this device" /> : (
        <ul class="list">
          {data.map((t) => (
            <li key={t.id}><Priority p={t.priority} /><div class="grow"><Link href={'/tickets/' + t.id} class="strong">#{t.number} {t.title}</Link><div class="muted small">updated {timeAgo(t.updated_at)}</div></div><StatusBadge s={t.status} /></li>
          ))}
        </ul>
      )}
    </Card>
  );
}

function DeviceSettings({ d, onSaved }: { d: any; onSaved: (d: any) => void }) {
  const clients = useFetch<any[]>('/clients');
  const [name, setName] = useState(d.display_name || '');
  const [notes, setNotes] = useState(d.notes || '');
  const [tags, setTags] = useState((d.tags || []).join(', '));
  const [clientId, setClientId] = useState(d.client_id || '');
  const [siteId, setSiteId] = useState(d.site_id || '');
  const [busy, setBusy] = useState(false);
  const sites = clients.data?.find((c) => c.id === clientId)?.sites || [];
  const save = async () => {
    setBusy(true);
    const body: any = { display_name: name, notes, tags: tags.split(',').map((t: string) => t.trim()).filter(Boolean) };
    if (clientId !== d.client_id) body.client_id = clientId;
    body.site_id = siteId;
    await attempt(async () => onSaved(await api.patch('/devices/' + d.id, body)));
    setBusy(false);
  };
  return (
    <Card title="Device details">
      <div class="form-grid">
        <Field label="Display name" hint={`Leave blank to use the hostname (${d.hostname})`}><input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} /></Field>
        <Field label="Tags" hint="Comma separated"><input value={tags} onInput={(e) => setTags((e.target as HTMLInputElement).value)} /></Field>
        <Field label="Client"><select value={clientId} onChange={(e) => { setClientId((e.target as HTMLSelectElement).value); setSiteId(''); }}>{clients.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
        <Field label="Site"><select value={siteId} onChange={(e) => setSiteId((e.target as HTMLSelectElement).value)}><option value="">—</option>{sites.map((s: any) => <option key={s.id} value={s.id}>{s.name}</option>)}</select></Field>
        <Field label="Notes"><textarea rows={5} value={notes} onInput={(e) => setNotes((e.target as HTMLTextAreaElement).value)} /></Field>
      </div>
      <div class="form-actions"><Button variant="primary" loading={busy} onClick={save}>Save changes</Button></div>
    </Card>
  );
}

function RustDeskModal({ d, onClose }: { d: any; onClose: () => void }) {
  const creds = useFetch<any>(`/devices/${d.id}/rustdesk`);
  const [busy, setBusy] = useState(false);
  const [out, setOut] = useState('');
  const provision = async () => {
    setBusy(true);
    setOut('');
    try {
      const r = await api.post(`/devices/${d.id}/rustdesk`);
      setOut(r.output || '');
      toast(r.id ? `RustDesk ready (ID ${r.id})` : 'RustDesk configured');
      creds.reload();
    } catch (e: any) {
      setOut(e.message);
      toast(e.message, 'error');
    }
    setBusy(false);
  };
  const c = creds.data;
  return (
    <Modal title="RustDesk (backup remote access)" onClose={onClose} width={600}
      footer={<>
        <Button onClick={onClose}>Close</Button>
        <Button icon="download" loading={busy} disabled={!d.online} onClick={provision}>{c?.id ? 'Re-apply configuration' : 'Install & configure RustDesk'}</Button>
        {c?.uri && <a class="btn btn-primary btn-md" href={c.uri}><Icon name="external" /><span>Connect with RustDesk</span></a>}
      </>}>
      <p class="muted">RustDesk runs as an independent fallback connection through your self-hosted RustDesk server. Use it if the built-in remote control can't reach a device (for example a Linux Wayland session).</p>
      {!c ? <Spinner /> : (
        <>
          {!c.server && <ErrorBox msg="RUSTDESK_HOST is not set on the server — see the README." />}
          <Kv items={[
            ['RustDesk ID', c.id ? <CopyText text={c.id} /> : <span class="muted">Not provisioned yet</span>],
            ['Password', c.password ? <CopyText text={c.password} /> : '—'],
            ['ID server', c.server || '—'],
            ['Server key', c.key ? <CopyText text={c.key} /> : '—'],
          ]} />
          <p class="muted small">"Connect" opens the RustDesk app on your computer. Install it from rustdesk.com and point it at <b>{c.server || 'your server'}</b> with the key above.</p>
        </>
      )}
      {busy && <Spinner label="Installing and configuring RustDesk on the device — this can take a few minutes…" />}
      {out && <pre class="output">{out}</pre>}
    </Modal>
  );
}
