import { Icon } from '../icons';
import { Link } from '../router';
import { Card, Empty, ErrorBox, Meter, OsBadge, PageHeader, Priority, Severity, Spinner, StatusBadge, timeAgo, useFetch } from '../ui';
import { useUser } from '../user';

function Stat({ label, value, sub, icon, tone, href }: { label: string; value: any; sub?: any; icon: string; tone?: string; href: string }) {
  return (
    <Link href={href} class={'stat ' + (tone ? 'stat-' + tone : '')}>
      <div class="stat-icon"><Icon name={icon} size={20} /></div>
      <div>
        <div class="stat-label">{label}</div>
        <div class="stat-value">{value}</div>
        {sub && <div class="stat-sub">{sub}</div>}
      </div>
    </Link>
  );
}

export function Dashboard() {
  const user = useUser();
  const { data, error } = useFetch('/dashboard', [], 20000);
  if (error) return <div class="page"><ErrorBox msg={error} /></div>;
  if (!data) return <div class="page"><Spinner /></div>;
  const d = data.devices;
  const hour = new Date().getHours();
  const greet = hour < 12 ? 'Good morning' : hour < 18 ? 'Good afternoon' : 'Good evening';
  return (
    <div class="page">
      <PageHeader title={`${greet}, ${(user.name || user.email).split(' ')[0]}`} subtitle="Here's what's happening across your fleet." />
      <div class="stats">
        <Stat href="/devices" icon="devices" label="Devices online" value={<>{d.online}<small> / {d.total}</small></>} sub={`${d.windows} Windows · ${d.mac} macOS · ${d.linux} Linux`} />
        <Stat href="/devices?status=offline" icon="power" label="Offline" value={d.offline} tone={d.offline > 0 ? 'warning' : undefined} sub={d.stressed > 0 ? `${d.stressed} under heavy load` : 'No devices under heavy load'} />
        <Stat href="/alerts" icon="bell" label="Open alerts" value={data.alerts.open} tone={data.alerts.critical > 0 ? 'critical' : data.alerts.open > 0 ? 'warning' : undefined} sub={`${data.alerts.critical} critical · ${data.alerts.warning} warning`} />
        <Stat href="/tickets" icon="ticket" label="Open tickets" value={data.tickets.open} sub={`${data.tickets.mine} assigned to you · ${data.tickets.unassigned} unassigned`} />
        <Stat href="/scripts?tab=jobs" icon="code" label="Script runs (24h)" value={data.jobs.success + data.jobs.failed + data.jobs.active} tone={data.jobs.failed > 0 ? 'warning' : undefined} sub={`${data.jobs.success} ok · ${data.jobs.failed} failed · ${data.jobs.active} running`} />
      </div>

      <div class="grid-2">
        <Card title="Active alerts" actions={<Link href="/alerts" class="link">View all</Link>} pad={false}>
          {data.recent_alerts.length === 0 ? <Empty icon="check" title="All clear">No open alerts.</Empty> : (
            <ul class="list">
              {data.recent_alerts.map((a: any) => (
                <li key={a.id}>
                  <Severity s={a.severity} />
                  <div class="grow">
                    <Link href={'/devices/' + a.device_id} class="strong">{a.title}</Link>
                    <div class="muted small">{a.device_name} · {timeAgo(a.created_at)}</div>
                  </div>
                  <StatusBadge s={a.status} />
                </li>
              ))}
            </ul>
          )}
        </Card>
        <Card title="Ticket queue" actions={<Link href="/tickets" class="link">View all</Link>} pad={false}>
          {data.recent_tickets.length === 0 ? <Empty icon="ticket" title="No open tickets" /> : (
            <ul class="list">
              {data.recent_tickets.map((t: any) => (
                <li key={t.id}>
                  <Priority p={t.priority} />
                  <div class="grow">
                    <Link href={'/tickets/' + t.id} class="strong">#{t.number} {t.title}</Link>
                    <div class="muted small">{t.client_name || 'No client'} · updated {timeAgo(t.updated_at)}</div>
                  </div>
                  <StatusBadge s={t.status} />
                </li>
              ))}
            </ul>
          )}
        </Card>
        <Card title="Busiest devices" pad={false}>
          {data.top_cpu.length === 0 ? <Empty icon="devices" title="No devices online">Deploy an agent from the Clients page.</Empty> : (
            <table class="table compact">
              <thead><tr><th>Device</th><th>CPU</th><th>Memory</th><th>Disk</th></tr></thead>
              <tbody>
                {data.top_cpu.map((x: any) => (
                  <tr key={x.id}>
                    <td><Link href={'/devices/' + x.id} class="strong">{x.name}</Link><div><OsBadge os={x.os} /></div></td>
                    <td><Meter value={x.cpu_pct} /></td>
                    <td><Meter value={x.mem_pct} /></td>
                    <td><Meter value={x.disk_pct} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>
        <Card title="Clients" actions={<Link href="/clients" class="link">Manage</Link>} pad={false}>
          {data.by_client.length === 0 ? <Empty icon="building" title="No clients" /> : (
            <ul class="list">
              {data.by_client.map((c: any) => (
                <li key={c.id}>
                  <div class="avatar sm">{c.name.slice(0, 1).toUpperCase()}</div>
                  <div class="grow">
                    <Link href={'/devices?client=' + c.id} class="strong">{c.name}</Link>
                    <div class="muted small">{c.devices} devices</div>
                  </div>
                  <span class="muted small">{c.online} online</span>
                </li>
              ))}
            </ul>
          )}
        </Card>
      </div>
    </div>
  );
}
