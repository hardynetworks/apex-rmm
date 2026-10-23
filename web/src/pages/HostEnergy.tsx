import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { api } from '../api';
import { LineChart } from '../chart';
import { Icon } from '../icons';
import { Link } from '../router';
import { Badge, Card, Empty, ErrorBox, Spinner } from '../ui';
import { fmtKWh, fmtW } from './Power';

// Virtualization → Energy: what each ESXi host draws right now and what it costs.
// vSphere refreshes host power every 20 s; the page ticks every second in between (the running
// costs keep counting at the latest draw) and asks the server again when the next reading is due.

const HOST_COLORS = ['#00d4ff', '#8b5cf6', '#10b981', '#f59e0b', '#ef4444', '#ec4899', '#06b6d4', '#84cc16'];
const WINDOWS: [number, string][] = [[120, '2 min'], [600, '10 min'], [3600, '1 hour']];
const SOURCE: Record<string, string> = { counter: 'vSphere power counter', sensor: 'IPMI power sensor', plug: 'Smart plug', estimate: 'Estimated from CPU load' };

type Point = { ts: string; t: number; total: number; [ref: string]: any };

function money(v: number, cur: string) {
  if (!isFinite(v)) return '—';
  const abs = Math.abs(v);
  // small running totals get more decimals so they visibly tick up
  const s = abs >= 1000 ? Math.round(v).toLocaleString() : abs >= 100 ? v.toFixed(0) : abs >= 1 ? v.toFixed(2) : abs >= 0.1 ? v.toFixed(3) : v.toFixed(4);
  return cur + s;
}

export function HostEnergy({ conn }: { conn: any }) {
  const [data, setData] = useState<any>(null);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(Date.now());
  const [win, setWin] = useState(() => { try { return Number(localStorage.getItem('apex-energy-window')) || 600; } catch { return 600; } });
  const points = useRef<Point[]>([]);
  const fetchedAt = useRef(0);

  // readings: ask again when the server says the next one is due
  useEffect(() => {
    let stop = false;
    let timer = 0;
    const load = async () => {
      try {
        const d = await api.get<any>(`/vsphere/connections/${conn.id}/energy`);
        if (stop) return;
        setData(d);
        setError(null);
        fetchedAt.current = Date.now();
        timer = window.setTimeout(load, Math.max(2000, Math.min(20000, (d.next_ms || 5000) + 500)));
      } catch (e: any) {
        if (stop) return;
        setError(e.message);
        timer = window.setTimeout(load, 10000);
      }
    };
    load();
    return () => { stop = true; clearTimeout(timer); };
  }, [conn.id]);

  // seed the chart with the last hour of stored readings
  useEffect(() => {
    points.current = [];
    api.get<any[]>(`/vsphere/connections/${conn.id}/power-history?minutes=60`).then((rows) => {
      const byTs = new Map<number, Record<string, number>>();
      for (const r of rows) {
        const t = Math.floor(new Date(r.ts).getTime() / 1000) * 1000;
        if (!byTs.has(t)) byTs.set(t, {});
        byTs.get(t)![r.host_ref] = r.watts;
      }
      const last: Record<string, number> = {};
      const seeded: Point[] = [];
      for (const t of [...byTs.keys()].sort((a, b) => a - b)) {
        Object.assign(last, byTs.get(t));
        const p: Point = { ts: new Date(t).toISOString(), t, total: 0 };
        for (const [ref, w] of Object.entries(last)) { p[ref] = w; p.total += w; }
        seeded.push(p);
      }
      points.current = [...seeded, ...points.current.filter((p) => !seeded.length || p.t > seeded[seeded.length - 1].t)];
    }).catch(() => {});
  }, [conn.id]);

  // tick every second: a new chart point at the latest draw, and the running costs move on
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);

  const hosts: any[] = data?.hosts || [];
  useEffect(() => {
    if (!data) return;
    const p: Point = { ts: new Date(now).toISOString(), t: now, total: 0 };
    for (const h of hosts) if (h.watts != null) { p[h.ref] = h.watts; p.total += h.watts; }
    const arr = points.current;
    arr.push(p);
    const cutoff = now - 3600e3;
    while (arr.length && arr[0].t < cutoff) arr.shift();
  }, [now, data]);

  const setWindow = (s: number) => { setWin(s); try { localStorage.setItem('apex-energy-window', String(s)); } catch { /* ignore */ } };

  const chart = useMemo(() => {
    const from = now - win * 1000;
    const inWin = points.current.filter((p) => p.t >= from);
    const step = Math.max(1, Math.ceil(inWin.length / 600)); // at most ~600 points on screen
    return inWin.filter((_, i) => i % step === 0 || i === inWin.length - 1);
  }, [now, win]);

  if (error && !data) return <ErrorBox msg={error} />;
  if (!data) return <Spinner label="Reading the hosts' power…" />;

  const rate: number = data.rate || 0;
  const cur: string = data.currency || '$';
  const at = new Date(data.at).getTime();
  const sinceH = Math.max(0, (now - at) / 3600e3); // hours since the reading the totals go up to
  const live = (kwh: number, w: number | null) => kwh + ((w || 0) * sinceH) / 1000;
  const withW = hosts.filter((h) => h.watts != null);
  const totalW = withW.reduce((s, h) => s + h.watts, 0);
  const sum = (f: (h: any) => number) => hosts.reduce((s, h) => s + f(h), 0);
  const todayKWh = sum((h) => live(h.today_kwh, h.watts));
  const monthKWh = sum((h) => live(h.month_kwh, h.watts));
  const hoursLeft = Math.max(0, (data.month_hours_left || 0) - (now - fetchedAt.current) / 3600e3);
  const projKWh = (h: any) => live(h.month_kwh, h.watts) + ((h.watts || 0) * hoursLeft) / 1000;
  const projMonthKWh = sum(projKWh);
  const estimated = hosts.filter((h) => h.source === 'estimate');
  const age = Math.max(0, Math.round((now - at) / 1000));
  const nextIn = Math.max(0, Math.round((at + 20000 - now) / 1000));
  const cost = (kwh: number) => (rate ? money(kwh * rate, cur) : fmtKWh(kwh));
  const color = (i: number) => HOST_COLORS[i % HOST_COLORS.length];
  const series = [
    { key: 'total', label: 'Total', color: 'var(--text-2)' },
    ...hosts.map((h, i) => ({ key: h.ref, label: h.name, color: color(i) })),
  ];

  if (!hosts.length) return <Card><Empty icon="bolt" title="No hosts">This connection has no ESXi hosts.</Empty></Card>;

  return (
    <>
      <div class="toolbar energy-bar">
        <span class={'live-pill' + (data.live ? '' : ' stale')} title={data.live_error || ''}>
          <span class="live-dot" /> {data.live ? 'Live' : 'Last poll'} · read {age < 2 ? 'just now' : `${age}s ago`}{data.live && <> · next in {nextIn || '…'}s</>}
        </span>
        <span class="grow" />
        <div class="seg">{WINDOWS.map(([s, l]) => <button key={s} class={win === s ? 'active' : ''} onClick={() => setWindow(s)}>{l}</button>)}</div>
      </div>
      {!rate && (
        <p class="callout small"><Icon name="info" size={14} /><span>Set your electricity price in <Link href="/settings?tab=general">Settings → General → Power &amp; energy</Link> to see costs. Until then energy is shown in kWh.</span></p>
      )}

      <div class="energy-totals energy-hero">
        <div><span class="muted small">Drawing now</span><strong>{fmtW(totalW)}</strong><span class="muted small">{withW.length} of {hosts.length} host{hosts.length === 1 ? '' : 's'}{estimated.length ? ` · ${estimated.length} estimated` : ''}</span></div>
        <div><span class="muted small">Cost per hour</span><strong>{rate ? money((totalW / 1000) * rate, cur) : fmtKWh(totalW / 1000)}</strong><span class="muted small">{rate ? `at ${cur}${rate}/kWh` : 'per hour'}</span></div>
        <div><span class="muted small">Today</span><strong class="tabnum">{cost(todayKWh)}</strong><span class="muted small">{fmtKWh(todayKWh)}</span></div>
        <div><span class="muted small">This month so far</span><strong class="tabnum">{cost(monthKWh)}</strong><span class="muted small">{fmtKWh(monthKWh)}</span></div>
        <div><span class="muted small">Projected this month</span><strong>{cost(projMonthKWh)}</strong><span class="muted small">at the current draw</span></div>
        <div><span class="muted small">Per year at this rate</span><strong>{cost((totalW * 8760) / 1000)}</strong><span class="muted small">{fmtKWh((totalW * 8760) / 1000)}</span></div>
      </div>

      <Card title="Live power draw" icon="activity" actions={<span class="muted small">updates every second</span>}>
        <LineChart data={chart} series={series} height={260} format={(v) => Math.round(v) + ' W'} empty="Waiting for the first reading…" />
      </Card>

      <Card title="Cost by server" icon="server" pad={false}>
        <div class="table-wrap">
          <table class="table energy-table">
            <thead>
              <tr>
                <th>Host</th><th>Now</th><th class="hide-sm">Share</th><th>Per hour</th><th>Today</th>
                <th class="hide-sm">This month</th><th class="hide-md">Projected month</th><th class="hide-md">Per year</th>
              </tr>
            </thead>
            <tbody>
              {hosts.map((h, i) => {
                const w = h.watts;
                const share = totalW > 0 && w != null ? (w / totalW) * 100 : 0;
                return (
                  <tr key={h.ref}>
                    <td>
                      <div class="dev-cell">
                        <span class="host-swatch" style={{ background: color(i) }} />
                        <div>
                          <span class="strong" title={h.model.trim()}>{h.name}</span>
                          <div class="muted small" title={h.note || ''}>
                            {h.source === 'estimate' ? <span class="text-warn">estimated · {Math.round(h.cpu_pct)}% CPU</span> : h.source === 'plug' && h.note ? h.note : SOURCE[h.source] || (h.state !== 'connected' ? 'not connected' : 'no reading')}
                          </div>
                        </div>
                      </div>
                    </td>
                    <td class="nowrap strong">{w != null ? fmtW(w) : '—'}</td>
                    <td class="hide-sm"><div class="share-bar"><div style={{ width: share + '%', background: color(i) }} /></div><span class="muted small">{Math.round(share)}%</span></td>
                    <td class="nowrap">{w != null ? (rate ? money((w / 1000) * rate, cur) : fmtKWh(w / 1000)) : '—'}</td>
                    <td class="nowrap tabnum">{cost(live(h.today_kwh, w))}</td>
                    <td class="nowrap tabnum hide-sm">{cost(live(h.month_kwh, w))}{h.month_est_kwh > 0 && <span class="muted small" title="Partly estimated"> ~</span>}</td>
                    <td class="nowrap hide-md">{cost(projKWh(h))}</td>
                    <td class="nowrap hide-md">{w != null ? cost((w * 8760) / 1000) : '—'}</td>
                  </tr>
                );
              })}
            </tbody>
            <tfoot>
              <tr>
                <td class="strong">Total</td><td class="nowrap strong">{fmtW(totalW)}</td><td class="hide-sm" />
                <td class="nowrap strong">{rate ? money((totalW / 1000) * rate, cur) : fmtKWh(totalW / 1000)}</td>
                <td class="nowrap strong tabnum">{cost(todayKWh)}</td><td class="nowrap strong tabnum hide-sm">{cost(monthKWh)}</td>
                <td class="nowrap strong hide-md">{cost(projMonthKWh)}</td><td class="nowrap strong hide-md">{cost((totalW * 8760) / 1000)}</td>
              </tr>
            </tfoot>
          </table>
        </div>
      </Card>

      {estimated.length > 0 && (
        <p class="callout warn small"><Icon name="info" size={14} /><span>
          <strong>{estimated.map((h) => h.name).join(', ')}</strong> {estimated.length === 1 ? "doesn't" : "don't"} report power through vSphere, so {estimated.length === 1 ? 'its draw is' : 'their draw is'} estimated from CPU load. {estimated[0].note && <>({estimated[0].note}.)</>}
        </span></p>
      )}
      <p class="muted small">Today and this month count from when Apex started recording these hosts' power. Projections assume the current draw continues. {data.live ? 'vSphere refreshes host power every 20 seconds.' : 'Live readings are unavailable right now; showing the last poll.'} <Badge>{conn.name}</Badge></p>
    </>
  );
}
