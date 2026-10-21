import { useMemo, useState } from 'preact/hooks';
import { Icon } from '../icons';
import { Link, navigate, useQuery } from '../router';
import { Fragment } from 'preact';
import { api } from '../api';
import { Badge, Button, Card, DEVICE_TYPES, Empty, ErrorBox, Meter, OsBadge, PageHeader, SearchInput, Spinner, StatusDot, TYPE_LABEL, attempt, deviceName, timeAgo, typeIcon, useFetch } from '../ui';
import { fmtW } from './Power';
import { useCan } from '../user';
import { RunScriptModal } from './Scripts';
import { DeployModal } from './Clients';

export function Devices() {
  const q = useQuery();
  const [search, setSearch] = useState(q.get('q') || '');
  const client = q.get('client') || '';
  const status = q.get('status') || '';
  const os = q.get('os') || '';
  const type = q.get('type') || '';
  const [grouped, setGrouped] = useState(() => { try { return localStorage.getItem('apex-devices-grouped') === '1'; } catch { return false; } });
  const toggleGrouped = () => { const v = !grouped; setGrouped(v); try { localStorage.setItem('apex-devices-grouped', v ? '1' : '0'); } catch { /* ignore */ } };
  const params = new URLSearchParams();
  if (client) params.set('client', client);
  if (status) params.set('status', status);
  if (os) params.set('os', os);
  const { data, error, loading, reload } = useFetch<any[]>('/devices?' + params.toString(), [], 15000);
  const clients = useFetch<any[]>('/clients');
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [runOpen, setRunOpen] = useState(false);
  const [deploy, setDeploy] = useState(false);
  const tech = useCan('technician');
  const admin = useCan('admin');

  const setParam = (k: string, v: string) => {
    const p = new URLSearchParams(location.search);
    if (v) p.set(k, v); else p.delete(k);
    navigate('/devices' + (p.toString() ? '?' + p : ''), true);
  };

  // search first, so the group counts follow it; then the chosen group
  const searched = useMemo(() => {
    const s = search.trim().toLowerCase();
    if (!data) return [];
    if (!s) return data;
    return data.filter((d) => [d.hostname, d.display_name, d.platform, d.client_name, d.public_ip, ...(d.local_ips || []), ...(d.logged_in_users || []), ...(d.tags || []), TYPE_LABEL[d.type]]
      .some((v) => (v || '').toLowerCase().includes(s)));
  }, [data, search]);
  const counts = useMemo(() => {
    const c: Record<string, number> = {};
    for (const d of searched) c[d.type] = (c[d.type] || 0) + 1;
    return c;
  }, [searched]);
  const rows = useMemo(() => (type ? searched.filter((d) => d.type === type) : searched), [searched, type]);
  const anyGPU = rows.some((d) => d.gpu_pct != null);
  const setType = async (t: string) => {
    const ids = [...selected];
    if (await attempt(async () => { for (const id of ids) await api.patch('/devices/' + id, { device_type: t }); }, `${ids.length} device${ids.length === 1 ? '' : 's'} moved to ${t ? TYPE_LABEL[t] + 's' : 'their detected group'}`)) {
      setSelected(new Set());
      reload();
    }
  };

  const toggle = (id: string) => {
    const n = new Set(selected);
    n.has(id) ? n.delete(id) : n.add(id);
    setSelected(n);
  };
  const allSel = rows.length > 0 && rows.every((r) => selected.has(r.id));

  return (
    <div class="page">
      <PageHeader
        title="Devices"
        subtitle={data ? `${data.filter((d) => d.online).length} of ${data.length} online` : ' '}
        actions={<>
          {tech && selected.size > 0 && (
            <select class="select-action" value="" onChange={(e) => { const v = (e.target as HTMLSelectElement).value; (e.target as HTMLSelectElement).value = ''; if (v) setType(v === 'auto' ? '' : v); }} aria-label="Set group">
              <option value="">Set group…</option>
              {DEVICE_TYPES.map(([v]) => <option key={v} value={v}>{TYPE_LABEL[v]}</option>)}
              <option value="auto">Detected automatically</option>
            </select>
          )}
          {tech && selected.size > 0 && <Button icon="play" variant="primary" onClick={() => setRunOpen(true)}>Run script on {selected.size}</Button>}
          {admin && <Button icon="plus" variant={selected.size > 0 ? 'default' : 'primary'} onClick={() => setDeploy(true)}>Add device</Button>}
        </>}
      />
      <div class="group-tabs" role="tablist" aria-label="Device groups">
        <button role="tab" aria-selected={!type} class={!type ? 'active' : ''} onClick={() => setParam('type', '')}><Icon name="devices" size={16} /> All <span class="count">{searched.length}</span></button>
        {DEVICE_TYPES.filter(([v]) => counts[v] || type === v).map(([v, label, icon]) => (
          <button key={v} role="tab" aria-selected={type === v} class={type === v ? 'active' : ''} onClick={() => setParam('type', v)}><Icon name={icon} size={16} /> {label} <span class="count">{counts[v] || 0}</span></button>
        ))}
        <label class="group-toggle"><input type="checkbox" checked={grouped} onChange={toggleGrouped} /> Group by type</label>
      </div>
      <div class="toolbar">
        <SearchInput value={search} onInput={setSearch} placeholder="Search hostname, IP, user, tag…" />
        <select value={client} onChange={(e) => setParam('client', (e.target as HTMLSelectElement).value)}>
          <option value="">All clients</option>
          {clients.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
        </select>
        <select value={status} onChange={(e) => setParam('status', (e.target as HTMLSelectElement).value)}>
          <option value="">Any status</option>
          <option value="online">Online</option>
          <option value="offline">Offline</option>
        </select>
        <select value={os} onChange={(e) => setParam('os', (e.target as HTMLSelectElement).value)}>
          <option value="">Any OS</option>
          <option value="windows">Windows</option>
          <option value="darwin">macOS</option>
          <option value="linux">Linux</option>
        </select>
        <Button icon="refresh" variant="ghost" onClick={() => reload()} title="Refresh" />
      </div>
      {error && <ErrorBox msg={error} />}
      <Card pad={false}>
        {loading && !data ? <Spinner /> : rows.length === 0 ? (
          <Empty icon="devices" title={data && data.length > 0 ? 'No devices match your filters' : 'No devices yet'}>
            {data && data.length === 0 && admin && <p>Create an install command and run it on a Windows, macOS or Linux machine.<br /><br /><Button variant="primary" icon="plus" onClick={() => setDeploy(true)}>Add device</Button></p>}
          </Empty>
        ) : (
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  {tech && <th class="w-check"><input type="checkbox" checked={allSel} onChange={() => setSelected(allSel ? new Set() : new Set(rows.map((r) => r.id)))} aria-label="Select all" /></th>}
                  <th>Device</th>
                  <th>Client</th>
                  <th>OS</th>
                  <th class="hide-sm">CPU</th>
                  <th class="hide-sm">Memory</th>
                  <th class="hide-sm">Disk</th>
                  {anyGPU && <th class="hide-md">GPU</th>}
                  <th class="hide-md">Power</th>
                  <th class="hide-md">User</th>
                  <th>Last seen</th>
                </tr>
              </thead>
              <tbody>
                {(grouped && !type ? DEVICE_TYPES.map(([v, label, icon]) => ({ v, label, icon, list: rows.filter((d) => d.type === v) })).filter((g) => g.list.length) : [{ v: '', label: '', icon: '', list: rows }]).map((g) => (
                <Fragment key={g.v}>
                {g.v && <tr class="group-row"><td colSpan={20}><Icon name={g.icon} size={15} /> {g.label} <span class="muted">{g.list.length}</span></td></tr>}
                {g.list.map((d) => (
                  <tr key={d.id} class={selected.has(d.id) ? 'selected' : ''}>
                    {tech && <td class="w-check"><input type="checkbox" checked={selected.has(d.id)} onChange={() => toggle(d.id)} aria-label={'Select ' + deviceName(d)} /></td>}
                    <td>
                      <div class="dev-cell">
                        <StatusDot online={d.online} label={false} />
                        <div>
                          <Link href={'/devices/' + d.id} class="strong">{deviceName(d)}</Link>
                          <div class="muted small">
                            <span title={TYPE_LABEL[d.type]}><Icon name={typeIcon(d.type)} size={12} /></span> {d.local_ips?.[0] || d.public_ip || ''}
                            {d.open_alerts > 0 && <> · <span class="text-warn"><Icon name="alert" size={12} /> {d.open_alerts}</span></>}
                            {d.maintenance_until && new Date(d.maintenance_until) > new Date() && <> · <Badge tone="warning">maintenance</Badge></>}
                          </div>
                        </div>
                      </div>
                    </td>
                    <td>{d.client_name || <span class="muted">—</span>}<div class="muted small">{d.site_name || ''}</div></td>
                    <td><OsBadge os={d.os} /><div class="muted small truncate" title={d.platform + ' ' + d.os_version}>{d.platform} {d.os_version?.split(' ')[0]}</div></td>
                    <td class="hide-sm">{d.online ? <Meter value={d.cpu_pct} /> : <span class="muted">—</span>}</td>
                    <td class="hide-sm">{d.online ? <Meter value={d.mem_pct} /> : <span class="muted">—</span>}</td>
                    <td class="hide-sm"><Meter value={d.disk_pct} /></td>
                    {anyGPU && <td class="hide-md">{d.gpu_pct != null ? <Meter value={d.gpu_pct} /> : <span class="muted">—</span>}</td>}
                    <td class="hide-md nowrap" title={d.power_kind === 'estimate' ? 'Estimate' : d.power_kind === 'partial' ? 'CPU/RAM/GPU only' : ''}>{d.power_w != null ? <>{fmtW(d.power_w)}{d.power_kind !== 'measured' && <span class="muted small"> {d.power_kind === 'partial' ? '*' : '~'}</span>}</> : <span class="muted">—</span>}</td>
                    <td class="hide-md truncate">{d.logged_in_users?.join(', ') || <span class="muted">—</span>}</td>
                    <td class="nowrap">{d.online ? <span class="text-good">Now</span> : timeAgo(d.last_seen)}</td>
                  </tr>
                ))}
                </Fragment>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
      {runOpen && <RunScriptModal deviceIds={[...selected]} onClose={() => setRunOpen(false)} />}
      {deploy && <DeployModal clients={clients.data || []} onClose={() => setDeploy(false)} />}
    </div>
  );
}
