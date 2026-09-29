import { useMemo, useState } from 'preact/hooks';
import { Icon } from '../icons';
import { Link, navigate, useQuery } from '../router';
import { Badge, Button, Card, Empty, ErrorBox, Meter, OsBadge, PageHeader, SearchInput, Spinner, StatusDot, deviceName, timeAgo, useFetch } from '../ui';
import { useCan } from '../user';
import { RunScriptModal } from './Scripts';
import { DeployModal } from './Clients';

export function Devices() {
  const q = useQuery();
  const [search, setSearch] = useState(q.get('q') || '');
  const client = q.get('client') || '';
  const status = q.get('status') || '';
  const os = q.get('os') || '';
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

  const rows = useMemo(() => {
    const s = search.trim().toLowerCase();
    if (!data) return [];
    if (!s) return data;
    return data.filter((d) => [d.hostname, d.display_name, d.platform, d.client_name, d.public_ip, ...(d.local_ips || []), ...(d.logged_in_users || []), ...(d.tags || [])]
      .some((v) => (v || '').toLowerCase().includes(s)));
  }, [data, search]);

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
          {tech && selected.size > 0 && <Button icon="play" variant="primary" onClick={() => setRunOpen(true)}>Run script on {selected.size}</Button>}
          {admin && <Button icon="plus" variant={selected.size > 0 ? 'default' : 'primary'} onClick={() => setDeploy(true)}>Add device</Button>}
        </>}
      />
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
                  <th class="hide-md">User</th>
                  <th>Last seen</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((d) => (
                  <tr key={d.id} class={selected.has(d.id) ? 'selected' : ''}>
                    {tech && <td class="w-check"><input type="checkbox" checked={selected.has(d.id)} onChange={() => toggle(d.id)} aria-label={'Select ' + deviceName(d)} /></td>}
                    <td>
                      <div class="dev-cell">
                        <StatusDot online={d.online} label={false} />
                        <div>
                          <Link href={'/devices/' + d.id} class="strong">{deviceName(d)}</Link>
                          <div class="muted small">
                            {d.local_ips?.[0] || d.public_ip || ''}
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
                    <td class="hide-md truncate">{d.logged_in_users?.join(', ') || <span class="muted">—</span>}</td>
                    <td class="nowrap">{d.online ? <span class="text-good">Now</span> : timeAgo(d.last_seen)}</td>
                  </tr>
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
