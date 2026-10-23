import { useMemo, useState } from 'preact/hooks';
import { StackSeries, StackedArea, StackedBars } from '../chart';
import { Icon } from '../icons';
import { Link, navigate, useQuery } from '../router';
import { Badge, Card, Empty, ErrorBox, PageHeader, SearchInput, Spinner, TYPE_LABEL, useFetch } from '../ui';
import { KindBadge, SOURCE_LABEL, fmtKWh, fmtW } from './Power';

// The Power page: all power use by device and ESXi host for a date range, with a PDF report.

const PALETTE = 8; // categorical slots (--cat-1 … --cat-8); the rest fold into "Other"
const TOP = PALETTE - 1;

type Range = { id: string; label: string };
const RANGES: Range[] = [
  { id: 'today', label: 'Today' },
  { id: '7d', label: '7 days' },
  { id: '30d', label: '30 days' },
  { id: 'month', label: 'This month' },
  { id: 'lastmonth', label: 'Last month' },
  { id: 'year', label: 'This year' },
  { id: 'custom', label: 'Custom' },
];

const iso = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

function rangeDates(id: string, from: string, to: string): [string, string] {
  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const minus = (n: number) => new Date(today.getFullYear(), today.getMonth(), today.getDate() - n);
  switch (id) {
    case 'today': return [iso(today), iso(today)];
    case '7d': return [iso(minus(6)), iso(today)];
    case 'month': return [iso(new Date(today.getFullYear(), today.getMonth(), 1)), iso(today)];
    case 'lastmonth': return [iso(new Date(today.getFullYear(), today.getMonth() - 1, 1)), iso(new Date(today.getFullYear(), today.getMonth(), 0))];
    case 'year': return [iso(new Date(today.getFullYear(), 0, 1)), iso(today)];
    case 'custom': return [from || iso(minus(29)), to || iso(today)];
    default: return [iso(minus(29)), iso(today)];
  }
}

function money(v: number | null | undefined, cur: string) {
  if (v == null) return null;
  return cur + (v >= 1000 ? Math.round(v).toLocaleString() : v.toFixed(2));
}

const fmtE = (v: number) => (v < 1 ? Math.round(v * 1000) + ' Wh' : (v >= 100 ? Math.round(v) : v.toFixed(v < 10 ? 1 : 0)) + ' kWh');
const shortDay = (d: any) => new Date(d.day + 'T12:00:00').toLocaleDateString(undefined, { month: 'short', day: 'numeric' });

type SortKey = 'name' | 'now_w' | 'avg_w' | 'kwh';

export function PowerPage() {
  const q = useQuery();
  const range = q.get('range') || '30d';
  const client = q.get('client') || '';
  const [from, to] = rangeDates(range, q.get('from') || '', q.get('to') || '');
  const setQ = (patch: Record<string, string>) => {
    const n = new URLSearchParams(q);
    for (const [k, v] of Object.entries(patch)) v ? n.set(k, v) : n.delete(k);
    navigate('/power?' + n.toString(), true);
  };
  const params = `from=${from}&to=${to}${client ? '&client=' + encodeURIComponent(client) : ''}`;
  const clients = useFetch<any[]>('/clients');
  const rep = useFetch<any>(`/power?${params}`, [params], 60000);
  const r = rep.data;

  // colors follow the source: the top 7 by energy keep slots 1-7 for this view, the rest are "Other"
  const top: any[] = useMemo(() => (r ? r.sources.filter((s: any) => s.kwh > 0 || s.now_w != null).slice(0, TOP) : []), [r]);
  const series: StackSeries[] = useMemo(() => {
    const s: StackSeries[] = top.map((x, i) => ({ key: x.key, label: x.name, color: `var(--cat-${i + 1})` }));
    if (r && r.sources.length > top.length) s.push({ key: 'other', label: `Other (${r.sources.length - top.length})`, color: 'var(--cat-other)' });
    return s;
  }, [top, r]);
  const keys = top.map((x) => x.key).join(',');
  const hist = useFetch<any>(r ? `/power/history?${params}&keys=${encodeURIComponent(keys)}` : null, [params, keys], 60000);

  // energy per day with the same series ("other" = everything else)
  const days = useMemo(() => {
    if (!r) return [];
    const keep = new Set(top.map((x) => x.key));
    return r.days.map((d: any) => {
      const by: Record<string, number> = {};
      for (const [k, v] of Object.entries(d.by as Record<string, number>)) {
        const kk = keep.has(k) ? k : 'other';
        by[kk] = (by[kk] || 0) + v;
      }
      return { ...d, by };
    });
  }, [r, top]);

  const pdf = `/api/power/report.pdf?${params}`;
  const clientList: any[] = clients.data || [];
  const clientName = clientList.find((c) => c.id === client)?.name;

  return (
    <div class="page">
      <PageHeader title="Power" subtitle="Energy use of every device and ESXi host" actions={
        <>
          <a class="btn btn-default btn-md" href={pdf + '&inline=1'} target="_blank" rel="noopener" title="Open the report in a new tab"><Icon name="external" size={17} /><span>View report</span></a>
          <a class="btn btn-primary btn-md" href={pdf} download><Icon name="download" size={17} /><span>Download PDF</span></a>
        </>
      } />
      <div class="power-filters">
        <div class="seg" role="group" aria-label="Date range">
          {RANGES.map((x) => (
            <button key={x.id} class={range === x.id ? 'active' : ''} onClick={() => setQ(x.id === 'custom' ? { range: 'custom', from, to } : { range: x.id, from: '', to: '' })}>{x.label}</button>
          ))}
        </div>
        {range === 'custom' && (
          <div class="inline-form">
            <input type="date" value={from} max={to} onChange={(e) => setQ({ from: (e.target as HTMLInputElement).value })} aria-label="From" />
            <span class="muted">to</span>
            <input type="date" value={to} min={from} onChange={(e) => setQ({ to: (e.target as HTMLInputElement).value })} aria-label="To" />
          </div>
        )}
        <select value={client} onChange={(e) => setQ({ client: (e.target as HTMLSelectElement).value })} aria-label="Client">
          <option value="">All clients</option>
          {clientList.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
        </select>
      </div>

      {rep.error && <ErrorBox msg={rep.error} />}
      {!r ? <Spinner /> : r.sources.length === 0 ? (
        <Card><Empty icon="bolt" title={clientName ? `No power readings for ${clientName}` : 'No power readings yet'}>
          Devices report power once their agent is up to date, and ESXi hosts once a vCenter connection is added under Virtualization.
        </Empty></Card>
      ) : (
        <div class="stack">
          <div class="energy-totals power-tiles">
            <div><span class="muted small">Drawing now</span><strong>{fmtW(r.now_w)}</strong><span class="muted small">{r.devices} device{r.devices === 1 ? '' : 's'} · {r.hosts} ESXi host{r.hosts === 1 ? '' : 's'}</span></div>
            <div><span class="muted small">Energy used</span><strong>{fmtKWh(r.kwh)}</strong><span class="muted small">{r.est_kwh > 0 && r.kwh > 0 ? `${Math.round((r.est_kwh / r.kwh) * 100)}% estimated` : 'measured'}</span></div>
            <div><span class="muted small">Cost</span><strong>{money(r.cost, r.currency) || '—'}</strong><span class="muted small">{r.rate ? `at ${r.currency}${r.rate} per kWh` : <Link href="/settings?tab=general">Set a price per kWh</Link>}</span></div>
            <div><span class="muted small">Average draw</span><strong>{fmtW(r.avg_w)}</strong><span class="muted small">{from === to ? from : `${shortDay({ day: from })} – ${shortDay({ day: to })}`}</span></div>
          </div>

          <Card title="Power over time" icon="activity" actions={<span class="muted small">{hist.data ? `${bucketLabel(hist.data.bucket_seconds)} averages` : ''}</span>}>
            {!hist.data ? <Spinner /> : (
              <>
                <StackedArea data={hist.data.points} series={series} format={(v) => fmtW(v).replace('.0 ', ' ')} empty="No power readings in this period" />
                <Legend series={series} />
                {new Date(from + 'T00:00:00') < new Date(hist.data.device_history_from) && (
                  <p class="muted small"><Icon name="info" size={13} /> Detailed readings are kept {retentionDays(hist.data.device_history_from)} days for devices and 30 days for ESXi hosts, so the start of this range may be missing here. Energy totals below cover the whole range.</p>
                )}
              </>
            )}
          </Card>

          <Card title="Energy per day" icon="bolt">
            <StackedBars data={days} series={series} label={shortDay} format={fmtE} empty="No energy recorded in this period" />
            <Legend series={series} />
          </Card>

          <SourcesTable r={r} colors={new Map(series.map((s) => [s.key, s.color]))} showClient={!client} />
        </div>
      )}
    </div>
  );
}

function bucketLabel(sec: number) {
  if (sec < 3600) return `${Math.round(sec / 60)}-minute`;
  if (sec < 86400) return `${Math.round(sec / 3600)}-hour`;
  return 'Daily';
}

function retentionDays(fromIso: string) {
  return Math.round((Date.now() - new Date(fromIso).getTime()) / 86400e3);
}

function Legend({ series }: { series: StackSeries[] }) {
  return (
    <div class="chart-legend">
      {series.map((s) => <span key={s.key} class="legend-item"><span class="swatch" style={{ background: s.color }} />{s.label}</span>)}
    </div>
  );
}

function SourcesTable({ r, colors, showClient }: { r: any; colors: Map<string, string>; showClient: boolean }) {
  const [search, setSearch] = useState('');
  const [kind, setKind] = useState<'all' | 'device' | 'host'>('all');
  const [sort, setSort] = useState<{ k: SortKey; desc: boolean }>({ k: 'kwh', desc: true });
  const rows = useMemo(() => {
    const s = search.trim().toLowerCase();
    const list = r.sources.filter((x: any) => (kind === 'all' || x.type === kind) && (!s || x.name.toLowerCase().includes(s) || x.client.toLowerCase().includes(s)));
    const val = (x: any) => (sort.k === 'name' ? x.name.toLowerCase() : x[sort.k] ?? -1);
    return [...list].sort((a, b) => {
      const va = val(a), vb = val(b);
      const c = va < vb ? -1 : va > vb ? 1 : 0;
      return sort.desc ? -c : c;
    });
  }, [r, search, kind, sort]);
  const maxShare = Math.max(1, ...r.sources.map((x: any) => x.share_pct));
  const th = (k: SortKey, label: string, cls = '') => (
    <th class={cls}>
      <button class={'th-sort ' + (sort.k === k ? 'on' : '')} onClick={() => setSort({ k, desc: sort.k === k ? !sort.desc : k !== 'name' })}
        aria-sort={sort.k === k ? (sort.desc ? 'descending' : 'ascending') : 'none'}>
        {label}{sort.k === k && <span class="th-arrow">{sort.desc ? '▾' : '▴'}</span>}
      </button>
    </th>
  );
  const nDev = r.sources.filter((x: any) => x.type === 'device').length;
  return (
    <Card title="By device and host" icon="list" pad={false} actions={
      <div class="inline" style="margin-top:0">
        <div class="seg">
          {([['all', `All ${r.sources.length}`], ['device', `Devices ${nDev}`], ['host', `ESXi hosts ${r.sources.length - nDev}`]] as const).map(([k, l]) => (
            <button key={k} class={kind === k ? 'active' : ''} onClick={() => setKind(k)}>{l}</button>
          ))}
        </div>
        <SearchInput value={search} onInput={setSearch} placeholder="Search…" />
      </div>
    }>
      <div class="table-wrap">
        <table class="table power-table">
          <thead>
            <tr>
              {th('name', 'Name')}
              {showClient && <th class="hide-md">Client</th>}
              <th class="hide-sm">Source</th>
              {th('now_w', 'Now', 'right')}
              {th('avg_w', 'Average', 'right hide-sm')}
              {th('kwh', 'Energy', 'right')}
              <th class="right hide-sm">Share</th>
              {r.rate > 0 && <th class="right">Cost</th>}
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && <tr><td colSpan={8} class="muted center">Nothing matches.</td></tr>}
            {rows.map((x: any) => (
              <tr key={x.key}>
                <td>
                  <div class="power-name">
                    <span class="swatch" style={{ background: colors.get(x.key) || 'var(--cat-other)' }} />
                    {x.type === 'host'
                      ? <Link href="/virtualization?tab=hosts" class="strong">{x.name}</Link>
                      : <Link href={`/devices/${x.id}?tab=power`} class="strong">{x.name}</Link>}
                    <span class="muted small">{x.type === 'host' ? 'ESXi host' : TYPE_LABEL[x.group] || x.group}</span>
                  </div>
                </td>
                {showClient && <td class="hide-md muted">{x.client || '—'}</td>}
                <td class="hide-sm"><span class="small">{SOURCE_LABEL[x.src] || x.src || '—'}</span> {x.kind !== 'measured' && <KindBadge kind={x.kind} />}</td>
                <td class="right nowrap">{x.now_w != null ? fmtW(x.now_w) : <span class="muted">{x.online ? '—' : 'offline'}</span>}</td>
                <td class="right nowrap hide-sm">{fmtW(x.avg_w)}</td>
                <td class="right nowrap"><strong>{fmtKWh(x.kwh)}</strong>{x.est_kwh > x.kwh / 2 && x.kwh > 0 && <span class="muted small" title="Mostly estimated"> est.</span>}</td>
                <td class="right hide-sm"><span class="share"><span class="share-bar" style={{ width: (x.share_pct / maxShare) * 100 + '%' }} /></span><span class="small">{x.share_pct.toFixed(1)}%</span></td>
                {r.rate > 0 && <td class="right nowrap">{money(x.cost, r.currency)}</td>}
              </tr>
            ))}
          </tbody>
          {rows.length > 1 && (
            <tfoot>
              <tr>
                <td><strong>{rows.length === r.sources.length ? 'Total' : `Total of ${rows.length} shown`}</strong></td>
                {showClient && <td class="hide-md" />}
                <td class="hide-sm" />
                <td class="right nowrap">{fmtW(rows.reduce((a: number, x: any) => a + (x.now_w || 0), 0))}</td>
                <td class="right nowrap hide-sm">{fmtW(rows.reduce((a: number, x: any) => a + x.avg_w, 0))}</td>
                <td class="right nowrap"><strong>{fmtKWh(rows.reduce((a: number, x: any) => a + x.kwh, 0))}</strong></td>
                <td class="right hide-sm small">{rows.reduce((a: number, x: any) => a + x.share_pct, 0).toFixed(1)}%</td>
                {r.rate > 0 && <td class="right nowrap"><strong>{money(rows.reduce((a: number, x: any) => a + (x.cost || 0), 0), r.currency)}</strong></td>}
              </tr>
            </tfoot>
          )}
        </table>
      </div>
      {r.sources.some((x: any) => x.type === 'host') && r.sources.some((x: any) => x.group === 'virtual' && x.kwh > 0) && (
        <p class="muted small card-note"><Badge>Note</Badge> Virtual machines with their own power estimate are also included in their ESXi host's draw.</p>
      )}
    </Card>
  );
}
