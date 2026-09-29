import { useEffect, useRef, useState } from 'preact/hooks';

export type Series = { key: string; label: string; color: string };

/** Time-series line chart with crosshair + tooltip. One y-axis only. */
export function LineChart({ data, series, height = 180, yMax, format = (v) => v.toFixed(1), empty = 'No data yet' }: {
  data: any[]; series: Series[]; height?: number; yMax?: number; format?: (v: number) => string; empty?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [w, setW] = useState(600);
  const [hover, setHover] = useState<number | null>(null);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver((e) => setW(Math.max(200, e[0].contentRect.width)));
    ro.observe(ref.current);
    return () => ro.disconnect();
  }, []);
  if (!data || data.length === 0) return <div class="chart-empty" style={{ height }}>{empty}</div>;

  const pad = { l: 40, r: 12, t: 10, b: 24 };
  const iw = w - pad.l - pad.r;
  const ih = height - pad.t - pad.b;
  const t0 = new Date(data[0].ts).getTime();
  const t1 = new Date(data[data.length - 1].ts).getTime();
  const span = Math.max(1, t1 - t0);
  let max = yMax ?? 0;
  if (yMax === undefined) for (const d of data) for (const s of series) max = Math.max(max, d[s.key] || 0);
  if (max <= 0) max = 1;
  const nice = yMax ?? niceMax(max);
  const x = (t: string) => pad.l + ((new Date(t).getTime() - t0) / span) * iw;
  const y = (v: number) => pad.t + ih - (Math.min(v, nice) / nice) * ih;
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * nice);
  const xticks = 5;
  const tfmt = span > 2 * 86400e3 ? { month: 'short', day: 'numeric' } : { hour: '2-digit', minute: '2-digit' };

  const onMove = (e: MouseEvent) => {
    const r = (e.currentTarget as SVGElement).getBoundingClientRect();
    const px = e.clientX - r.left;
    const t = t0 + ((px - pad.l) / iw) * span;
    let best = 0;
    let bd = Infinity;
    data.forEach((d, i) => {
      const dd = Math.abs(new Date(d.ts).getTime() - t);
      if (dd < bd) { bd = dd; best = i; }
    });
    setHover(best);
  };
  const hd = hover !== null ? data[hover] : null;
  const hx = hd ? x(hd.ts) : 0;

  return (
    <div class="chart" ref={ref}>
      <svg width={w} height={height} onMouseMove={onMove as any} onMouseLeave={() => setHover(null)} role="img">
        {ticks.map((v) => (
          <g key={v}>
            <line x1={pad.l} x2={w - pad.r} y1={y(v)} y2={y(v)} class="grid" />
            <text x={pad.l - 6} y={y(v) + 4} class="axis" text-anchor="end">{format(v).replace(/\.0(?=\D|$)/, '')}</text>
          </g>
        ))}
        {Array.from({ length: xticks }, (_, i) => {
          const t = t0 + (span * i) / (xticks - 1);
          const xx = pad.l + (iw * i) / (xticks - 1);
          return <text key={i} x={xx} y={height - 6} class="axis" text-anchor={i === 0 ? 'start' : i === xticks - 1 ? 'end' : 'middle'}>{new Date(t).toLocaleString(undefined, tfmt as any)}</text>;
        })}
        {series.map((s) => {
          const pts = data.map((d) => `${x(d.ts).toFixed(1)},${y(d[s.key] || 0).toFixed(1)}`);
          const area = `M${pad.l},${pad.t + ih} L${pts.join(' L')} L${x(data[data.length - 1].ts)},${pad.t + ih}Z`;
          return (
            <g key={s.key}>
              {series.length === 1 && <path d={area} fill={s.color} opacity="0.1" />}
              <polyline points={pts.join(' ')} fill="none" stroke={s.color} stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
            </g>
          );
        })}
        {hd && (
          <g>
            <line x1={hx} x2={hx} y1={pad.t} y2={pad.t + ih} class="crosshair" />
            {series.map((s) => <circle key={s.key} cx={hx} cy={y(hd[s.key] || 0)} r="4" fill={s.color} stroke="var(--surface)" stroke-width="2" />)}
          </g>
        )}
      </svg>
      {hd && (
        <div class="chart-tip" style={{ left: Math.min(hx + 12, w - 170) + 'px', top: '8px' }}>
          <div class="muted">{new Date(hd.ts).toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' })}</div>
          {series.map((s) => (
            <div key={s.key} class="tip-row">
              <span class="swatch" style={{ background: s.color }} />
              {series.length > 1 && <span>{s.label}</span>}
              <strong>{format(hd[s.key] || 0)}</strong>
            </div>
          ))}
        </div>
      )}
      {series.length > 1 && (
        <div class="legend">
          {series.map((s) => (
            <span key={s.key}><span class="swatch" style={{ background: s.color }} />{s.label}</span>
          ))}
        </div>
      )}
    </div>
  );
}

function niceMax(v: number) {
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}
