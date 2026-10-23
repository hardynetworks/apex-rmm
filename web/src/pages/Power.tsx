import { useState } from 'preact/hooks';
import { api } from '../api';
import { BarChart, LineChart } from '../chart';
import { Icon } from '../icons';
import { Link } from '../router';
import { Badge, Button, Card, Empty, Field, Kv, Spinner, attempt, useFetch } from '../ui';
import { DevicePlugCard } from './SmartPlugs';

// Power monitoring: what each device draws, its daily kWh, and the cost at the configured rate.

export const SOURCE_LABEL: Record<string, string> = {
  plug: 'Smart plug',
  ups: 'UPS',
  ipmi: 'Server management board (IPMI)',
  meter: 'Power meter (ACPI)',
  battery: 'Battery discharge rate',
  telemetry: 'Mac power telemetry',
  rapl: 'CPU energy counters',
  powermetrics: 'powermetrics',
  nvidia: 'NVIDIA driver',
  hwmon: 'GPU sensor',
  estimate: 'Estimate from CPU load',
  vsphere: 'VMware vSphere host',
  matter: 'Matter smart plug',
  tapo: 'Tapo smart plug',
};

export function fmtW(w: number | null | undefined) {
  if (w == null) return '—';
  if (w >= 10000) return (w / 1000).toFixed(1) + ' kW';
  return (w >= 100 ? Math.round(w) : Math.round(w * 10) / 10) + ' W';
}

export function fmtKWh(k: number | null | undefined) {
  if (k == null) return '—';
  if (k < 1) return Math.round(k * 1000) + ' Wh';
  return (k >= 100 ? Math.round(k) : k.toFixed(k >= 10 ? 1 : 2)) + ' kWh';
}

export function fmtCost(kwh: number, rate: number, cur: string) {
  if (!rate) return null;
  const v = kwh * rate;
  return cur + (v >= 100 ? Math.round(v).toLocaleString() : v.toFixed(2));
}

export function KindBadge({ kind }: { kind: string }) {
  if (kind === 'measured') return <Badge tone="good">Measured</Badge>;
  if (kind === 'partial') return <span title="Only some components (CPU, RAM, GPU) are measured; the rest of the machine isn't."><Badge tone="warning">Components only</Badge></span>;
  if (kind === 'estimate') return <span title="Worked out from CPU load; set the idle and full-load watts in Power settings to make it closer."><Badge>Estimate</Badge></span>;
  return null;
}

const shortDay = (d: any) => new Date(d.day + 'T12:00:00').toLocaleDateString(undefined, { month: 'short', day: 'numeric' });

function fmtRuntime(s: number) {
  if (s == null || s < 0) return '—';
  const m = Math.round(s / 60);
  return m >= 60 ? `${Math.floor(m / 60)} h ${m % 60} min` : `${m} min`;
}

function UPSKv({ u }: { u: any }) {
  return (
    <Kv items={[
      ['Status', <span class="title-row">{u.on_battery ? <Badge tone="critical">On battery</Badge> : <Badge tone="good">On mains</Badge>} <span class="muted small mono">{u.status}</span></span>],
      ['UPS', [u.name, u.model].filter(Boolean).join(' · ')],
      ['Battery', u.charge >= 0 ? Math.round(u.charge) + ' %' : '—'],
      ['Runtime left', fmtRuntime(u.runtime)],
      ['Load', u.load >= 0 ? `${Math.round(u.load)} %${u.w ? ' · ' + fmtW(u.w) + (u.w_estimated ? ' (est.)' : '') : ''}` : '—'],
      ...(u.nominal_w ? [['Capacity', fmtW(u.nominal_w)] as [string, any]] : []),
    ]} />
  );
}

/** The device page's Power tab. */
export function DevicePower({ d, tech, onSaved }: { d: any; tech: boolean; onSaved: (d: any) => void }) {
  const [hours, setHours] = useState(24);
  const [days, setDays] = useState(30);
  const energy = useFetch<any>(`/devices/${d.id}/energy?days=${days}`, [days], 60000);
  const metrics = useFetch<any[]>(`/devices/${d.id}/metrics?hours=${hours}`, [hours], 60000);
  const info = d.power_info || {};
  const parts: any[] = info.parts || [];
  const e = energy.data;
  const cost = (k: number) => (e ? fmtCost(k, e.rate, e.currency) : null);
  const hasPower = d.power_kind !== '' && d.power_kind != null;
  return (
    <div class="grid-2">
      <Card title="Power now" icon="bolt">
        {!d.online ? <Empty icon="bolt" title="Device is offline" /> : !hasPower ? (
          <Empty icon="bolt" title={d.type === 'virtual' ? 'Virtual machine' : 'No power readings yet'}>{d.energy_conf?.mode === 'off' ? 'Power monitoring is turned off for this device.'
            : d.type === 'virtual' ? "A VM's power is part of its host's, so it isn't estimated here (vSphere hosts are counted on the Power card). Choose Estimate in Power settings to estimate it anyway."
            : 'Update the agent to start power monitoring (⋯ → Update agent).'}</Empty>
        ) : (
          <>
            <div class="power-now">
              <Icon name="bolt" size={28} />
              <span class="power-big">{fmtW(d.power_w)}</span>
              <KindBadge kind={d.power_kind} />
            </div>
            <Kv items={[
              ['Source', SOURCE_LABEL[d.power_src] || d.power_src || '—'],
              ...(d.power_kind === 'partial' ? [['Note', 'Only the parts listed below are measured. Add the rest under Power settings → Extra watts.'] as [string, any]] : []),
              ...(info.note ? [['Problem', <span class="text-warn">{info.note}</span>] as [string, any]] : []),
            ]} />
            {parts.length > 0 && (
              <table class="table compact" style={{ marginTop: '10px' }}>
                <thead><tr><th>Component</th><th>Draw</th><th>Source</th></tr></thead>
                <tbody>{parts.map((p) => <tr key={p.name}><td>{p.name}</td><td>{fmtW(p.w)}</td><td class="muted small">{SOURCE_LABEL[p.src] || p.src}</td></tr>)}</tbody>
              </table>
            )}
          </>
        )}
      </Card>
      <DevicePlugCard deviceId={d.id} />
      <Card title="Energy use" icon="activity" actions={
        <div class="seg">{[[7, '7d'], [30, '30d'], [90, '90d']].map(([n, l]) => <button key={n} class={days === n ? 'active' : ''} onClick={() => setDays(n as number)}>{l}</button>)}</div>
      }>
        {!e ? <Spinner /> : (
          <>
            <div class="energy-totals">
              <div><span class="muted small">Today</span><strong>{fmtKWh(e.today_kwh)}</strong><span class="muted small">{cost(e.today_kwh) || ''}</span></div>
              <div><span class="muted small">This month</span><strong>{fmtKWh(e.month_kwh)}</strong><span class="muted small">{cost(e.month_kwh) || ''}</span></div>
              <div><span class="muted small">Last {days} days</span><strong>{fmtKWh(e.period_kwh)}</strong><span class="muted small">{cost(e.period_kwh) || ''}</span></div>
            </div>
            <BarChart data={e.days} value="kwh" part="est_kwh" label={shortDay} format={(v) => (v < 1 ? Math.round(v * 1000) + ' Wh' : v.toFixed(v < 10 ? 1 : 0) + ' kWh')} empty="No energy recorded yet" />
            {!e.rate && <p class="muted small">Set your electricity rate in Settings (Power & energy) to see costs.</p>}
            {e.month_est_kwh > 0 && <p class="muted small">Lighter parts of the bars are estimates or component-only readings.</p>}
          </>
        )}
      </Card>
      <Card title="Power draw" icon="gauge" class="span-2" actions={
        <div class="seg">{[[6, '6h'], [24, '24h'], [168, '7d'], [336, '14d']].map(([h, l]) => <button key={h} class={hours === h ? 'active' : ''} onClick={() => setHours(h as number)}>{l}</button>)}</div>
      }>
        <LineChart data={(metrics.data || []).filter((m) => m.power_w != null)} series={[{ key: 'power_w', label: 'Power', color: 'var(--series-2)' }]} format={(v) => Math.round(v) + ' W'} empty="No power readings in this period" />
      </Card>
      {info.ups && <Card title="UPS" icon="battery"><UPSKv u={info.ups} /></Card>}
      {tech && <PowerSettings d={d} onSaved={onSaved} />}
    </div>
  );
}

const MODES: [string, string, string][] = [
  ['auto', 'Automatic', 'Best source this device has: management board (IPMI), power meter, battery, then CPU/RAM/GPU counters, then an estimate.'],
  ['plug', 'Smart plug', 'Read a Shelly, Tasmota or Kasa plug (or any JSON endpoint) that this device is plugged into. The agent reads it over your network.'],
  ['ups', 'UPS', "Use the UPS's output power (NUT or apcupsd). Set a share if other equipment is on the same UPS."],
  ['estimate', 'Estimate', 'Work it out from CPU load between idle and full-load watts.'],
  ['off', 'Off', "Don't report power for this device."],
];

function num(v: any) {
  const n = parseFloat(v);
  return isFinite(n) ? n : 0;
}

function PowerSettings({ d, onSaved }: { d: any; onSaved: (d: any) => void }) {
  const init = { mode: 'auto', plug_type: 'shelly', plug_addr: '', plug_field: '', ups_addr: '', ups_name: '', ups_share: 100, idle_w: 0, max_w: 0, extra_w: 0, ...(d.energy_conf || {}) };
  if (!init.ups_share) init.ups_share = 100;
  const [c, setC] = useState<any>(init);
  const [busy, setBusy] = useState(false);
  const set = (k: string, v: any) => setC({ ...c, [k]: v });
  const inp = (k: string, props: any = {}) => <input value={c[k] === 0 ? '' : c[k] ?? ''} onInput={(e) => set(k, (e.target as HTMLInputElement).value)} {...props} />;
  const save = async () => {
    setBusy(true);
    const body = { ...c, ups_share: num(c.ups_share), idle_w: num(c.idle_w), max_w: num(c.max_w), extra_w: num(c.extra_w) };
    await attempt(async () => onSaved(await api.patch('/devices/' + d.id, { energy_conf: body })), 'Power settings saved');
    setBusy(false);
  };
  const mode = MODES.find((m) => m[0] === c.mode) || MODES[0];
  return (
    <Card title="Power settings" icon="settings" class="span-2">
      <div class="form-grid">
        <Field label="Measure with" hint={mode[2]}>
          <select value={c.mode} onChange={(e) => set('mode', (e.target as HTMLSelectElement).value)}>{MODES.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select>
        </Field>
        {c.mode === 'plug' && <>
          <Field label="Plug type" hint={c.plug_type === 'kasa' ? 'Kasa plugs with energy monitoring on the local protocol (HS110, KP115, KP125…). Newer firmware that requires a TP-Link login isn’t supported.' : c.plug_type === 'json' ? 'Any URL that returns JSON with the watts in it (Home Assistant, ESPHome, a script…).' : 'Shelly Plug/Plus/Pro/Gen3/Gen4 and Gen1 devices with power metering.'}>
            <select value={c.plug_type} onChange={(e) => set('plug_type', (e.target as HTMLSelectElement).value)}>
              <option value="shelly">Shelly</option><option value="tasmota">Tasmota</option><option value="kasa">TP-Link Kasa</option><option value="json">Other (JSON URL)</option>
            </select>
          </Field>
          <Field label={c.plug_type === 'json' ? 'URL' : 'Plug address'} hint="Reachable from this device, e.g. 192.168.1.50">{inp('plug_addr', { placeholder: c.plug_type === 'json' ? 'http://192.168.1.50/sensor/power' : '192.168.1.50' })}</Field>
          {c.plug_type === 'json' && <Field label="Field" hint='Dotted path to the watts, e.g. "power" or "data.0.watts"'>{inp('plug_field', { placeholder: 'power' })}</Field>}
        </>}
        {(c.mode === 'ups' || c.mode === 'auto') && <>
          <Field label="UPS server" hint="NUT (port 3493) or apcupsd (port 3551). Leave empty for this computer.">{inp('ups_addr', { placeholder: 'this computer' })}</Field>
          <Field label="UPS name (NUT)" hint="Leave empty for the first UPS">{inp('ups_name', { placeholder: 'first UPS' })}</Field>
        </>}
        {c.mode === 'ups' && <Field label="Share of the UPS load (%)" hint="If several machines share the UPS, the part that is this device">{inp('ups_share', { type: 'number', min: 1, max: 100 })}</Field>}
        {(c.mode === 'estimate' || c.mode === 'auto') && <>
          <Field label="Idle watts" hint="Used for estimates; empty = guess">{inp('idle_w', { type: 'number', min: 0, placeholder: 'auto' })}</Field>
          <Field label="Full-load watts" hint="Used for estimates; empty = guess">{inp('max_w', { type: 'number', min: 0, placeholder: 'auto' })}</Field>
        </>}
        {c.mode !== 'off' && c.mode !== 'plug' && c.mode !== 'ups' && <Field label="Extra watts" hint="Added to component-only readings and estimates (drives, fans, monitor…)">{inp('extra_w', { type: 'number', min: 0, placeholder: '0' })}</Field>}
      </div>
      <div class="form-actions"><Button variant="primary" loading={busy} onClick={save}>Save power settings</Button></div>
    </Card>
  );
}

/** Fleet power for the dashboard. */
export function FleetPowerCard() {
  const { data: e } = useFetch<any>('/energy?days=30', [], 30000);
  if (!e) return <Card title="Power"><Spinner /></Card>;
  const cost = (k: number) => fmtCost(k, e.rate, e.currency);
  const n = e.now;
  return (
    <Card title="Power" icon="bolt" class="span-2" actions={<Link href="/power" class="btn btn-ghost btn-sm">All power use &amp; reports</Link>}>
      {n.devices === 0 && !n.hosts && e.top.length === 0 ? (
        <Empty icon="bolt" title="No power readings yet">Agents report power after they're updated (device ⋯ → Update agent).</Empty>
      ) : (
        <>
          <div class="energy-totals">
            <div><span class="muted small">Drawing now</span><strong>{fmtW(n.watts)}</strong><span class="muted small">{n.devices} device{n.devices === 1 ? '' : 's'}{n.measured < n.devices ? ` · ${n.measured} measured` : ''}{n.hosts ? ` · ${n.hosts} vSphere host${n.hosts === 1 ? '' : 's'}${n.hosts_estimated ? ` (${n.hosts_estimated} est.)` : ''}` : ''}</span></div>
            <div><span class="muted small">Today</span><strong>{fmtKWh(e.today_kwh)}</strong><span class="muted small">{cost(e.today_kwh) || ''}</span></div>
            <div><span class="muted small">This month</span><strong>{fmtKWh(e.month_kwh)}</strong><span class="muted small">{cost(e.month_kwh) || ''}</span></div>
            <div><span class="muted small">Last 30 days</span><strong>{fmtKWh(e.period_kwh)}</strong><span class="muted small">{cost(e.period_kwh) || ''}</span></div>
          </div>
          {e.ups.length > 0 && (
            <div class="ups-strip">
              {e.ups.map((u: any) => (
                <Link key={u.id} href={`/devices/${u.id}?tab=power`} class={'ups-chip ' + (u.ups.on_battery ? 'ups-batt' : '')}>
                  <Icon name="battery" size={15} /> {u.ups.name} <span class="muted">via {u.device}</span>
                  <strong>{u.ups.on_battery ? 'On battery' : 'Mains'}</strong>
                  <span>{u.ups.charge >= 0 ? Math.round(u.ups.charge) + ' %' : ''}{u.ups.w ? ' · ' + fmtW(u.ups.w) : ''}</span>
                </Link>
              ))}
            </div>
          )}
          <div class="grid-2 fleet-power">
            <BarChart data={e.days} value="kwh" part="est_kwh" label={shortDay} format={(v) => (v < 1 ? Math.round(v * 1000) + ' Wh' : v.toFixed(v < 10 ? 1 : 0) + ' kWh')} empty="No energy recorded yet" />
            <table class="table compact">
              <thead><tr><th>Device</th><th>Now</th><th>This month</th></tr></thead>
              <tbody>
                {e.top.map((x: any) => (
                  <tr key={x.id || x.connection_id + x.host_ref}>
                    <td>{x.type === 'vsphere'
                      ? <><Link href="/virtualization?tab=hosts" class="strong">{x.name}</Link> <span class="muted small">ESXi host{x.power_kind === 'estimate' ? ' · est.' : ''}</span></>
                      : <><Link href={`/devices/${x.id}?tab=power`} class="strong">{x.name}</Link> {x.power_kind !== 'measured' && <span class="muted small">{x.power_kind === 'partial' ? 'components' : 'est.'}</span>}</>}</td>
                    <td>{x.power_w != null ? fmtW(x.power_w) : <span class="muted">—</span>}</td>
                    <td>{fmtKWh(x.month_kwh)} <span class="muted small">{cost(x.month_kwh) || ''}</span></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {!e.rate && <p class="muted small">Set your electricity rate in Settings (Power & energy) to see costs.</p>}
        </>
      )}
    </Card>
  );
}
