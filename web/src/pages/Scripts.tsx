import { useMemo, useState } from 'preact/hooks';
import { api } from '../api';
import { Icon } from '../icons';
import { Link, navigate, useQuery } from '../router';
import {
  Badge, Button, Card, Empty, ErrorBox, Field, Modal, OsBadge, PageHeader, SearchInput, Spinner, StatusBadge, Tabs,
  attempt, confirmDialog, fmtDate, timeAgo, toast, useFetch,
} from '../ui';
import { useCan } from '../user';

export const SHELLS: [string, string][] = [
  ['powershell', 'PowerShell'], ['pwsh', 'PowerShell 7'], ['cmd', 'Batch (cmd)'], ['bash', 'Bash'], ['sh', 'POSIX sh'], ['zsh', 'zsh'], ['python', 'Python'],
];
const shellLabel = (s: string) => SHELLS.find((x) => x[0] === s)?.[1] || s;
const defaultShell = (os?: string) => (os === 'windows' ? 'powershell' : os === 'darwin' ? 'zsh' : 'bash');

export function Scripts() {
  const q = useQuery();
  const tab = q.get('tab') || 'library';
  return (
    <div class="page">
      <PageHeader title="Automation" subtitle="Script library, run history and scheduled tasks." />
      <Tabs tabs={[{ id: 'library', label: 'Script library', icon: 'code' }, { id: 'jobs', label: 'Run history', icon: 'history' }, { id: 'schedules', label: 'Schedules', icon: 'calendar' }]}
        active={tab} onChange={(t) => navigate('/scripts?tab=' + t, true)} />
      {tab === 'library' && <Library />}
      {tab === 'jobs' && <Jobs />}
      {tab === 'schedules' && <Schedules />}
    </div>
  );
}

function Library() {
  const { data, error, reload } = useFetch<any[]>('/scripts');
  const [s, setS] = useState('');
  const [edit, setEdit] = useState<any | null>(null);
  const [run, setRun] = useState<any | null>(null);
  const tech = useCan('technician');
  const groups = useMemo(() => {
    const g: Record<string, any[]> = {};
    (data || []).filter((x) => !s || (x.name + x.description + x.category).toLowerCase().includes(s.toLowerCase())).forEach((x) => (g[x.category] ||= []).push(x));
    return Object.entries(g);
  }, [data, s]);
  const openEdit = async (id: string) => setEdit(await api.get('/scripts/' + id));
  return (
    <>
      <div class="toolbar">
        <SearchInput value={s} onInput={setS} placeholder="Search scripts" />
        <div class="grow" />
        {tech && <Button variant="primary" icon="plus" onClick={() => setEdit({ shell: 'powershell', platforms: ['windows'], timeout_seconds: 300, category: 'General', body: '' })}>New script</Button>}
      </div>
      {error && <ErrorBox msg={error} />}
      {!data ? <Spinner /> : groups.length === 0 ? <Card><Empty icon="code" title="No scripts" /></Card> : groups.map(([cat, list]) => (
        <Card key={cat} title={cat} pad={false}>
          <table class="table">
            <tbody>
              {list.map((x) => (
                <tr key={x.id}>
                  <td><button class="linklike strong" onClick={() => openEdit(x.id)}>{x.name}</button><div class="muted small">{x.description}</div></td>
                  <td class="nowrap"><Badge tone="neutral">{shellLabel(x.shell)}</Badge></td>
                  <td class="hide-sm">{(x.platforms || []).map((p: string) => <OsBadge key={p} os={p} />)}</td>
                  <td class="hide-md muted small nowrap">updated {timeAgo(x.updated_at)}</td>
                  <td class="right nowrap">{tech && <Button size="sm" icon="play" onClick={() => setRun(x)}>Run</Button>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      ))}
      {edit && <ScriptEditor script={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload(true); }} />}
      {run && <RunScriptModal scriptId={run.id} onClose={() => setRun(null)} />}
    </>
  );
}

function ScriptEditor({ script, onClose, onSaved }: { script: any; onClose: () => void; onSaved: () => void }) {
  const [s, setS] = useState({ ...script });
  const [busy, setBusy] = useState(false);
  const tech = useCan('technician');
  const set = (k: string, v: any) => setS({ ...s, [k]: v });
  const togglePlat = (p: string) => set('platforms', s.platforms.includes(p) ? s.platforms.filter((x: string) => x !== p) : [...s.platforms, p]);
  const save = async () => {
    setBusy(true);
    const ok = await attempt(() => (s.id ? api.put('/scripts/' + s.id, s) : api.post('/scripts', s)), 'Script saved');
    setBusy(false);
    if (ok) onSaved();
  };
  const del = async () => {
    if (!(await confirmDialog({ title: `Delete "${s.name}"?`, confirm: 'Delete', danger: true }))) return;
    if (await attempt(() => api.del('/scripts/' + s.id), 'Script deleted')) onSaved();
  };
  return (
    <Modal title={s.id ? 'Edit script' : 'New script'} onClose={onClose} width={860}
      footer={<>
        {s.id && tech && <Button variant="danger" icon="trash" onClick={del}>Delete</Button>}
        <div class="grow" />
        <Button onClick={onClose}>Cancel</Button>
        {tech && <Button variant="primary" loading={busy} onClick={save}>Save script</Button>}
      </>}>
      <div class="form-grid">
        <Field label="Name"><input value={s.name || ''} onInput={(e) => set('name', (e.target as HTMLInputElement).value)} /></Field>
        <Field label="Category"><input value={s.category || ''} onInput={(e) => set('category', (e.target as HTMLInputElement).value)} /></Field>
        <Field label="Description"><input value={s.description || ''} onInput={(e) => set('description', (e.target as HTMLInputElement).value)} /></Field>
        <div class="row-fields">
          <Field label="Language"><select value={s.shell} onChange={(e) => set('shell', (e.target as HTMLSelectElement).value)}>{SHELLS.map(([v, l]) => <option key={v} value={v}>{l}</option>)}</select></Field>
          <Field label="Timeout (s)"><input type="number" min={5} value={s.timeout_seconds} onInput={(e) => set('timeout_seconds', +(e.target as HTMLInputElement).value)} /></Field>
        </div>
        <Field label="Platforms">
          <div class="checks">
            {[['windows', 'Windows'], ['darwin', 'macOS'], ['linux', 'Linux']].map(([p, l]) => (
              <label key={p} class="check"><input type="checkbox" checked={s.platforms?.includes(p)} onChange={() => togglePlat(p)} /> {l}</label>
            ))}
          </div>
        </Field>
      </div>
      <Field label="Script" hint="Runs as SYSTEM on Windows and root on macOS/Linux.">
        <textarea class="code" rows={16} spellcheck={false} value={s.body} onInput={(e) => set('body', (e.target as HTMLTextAreaElement).value)}
          onKeyDown={(e) => { if (e.key === 'Tab') { e.preventDefault(); const t = e.target as HTMLTextAreaElement; const p = t.selectionStart; set('body', s.body.slice(0, p) + '  ' + s.body.slice(t.selectionEnd)); requestAnimationFrame(() => t.setSelectionRange(p + 2, p + 2)); } }} />
      </Field>
    </Modal>
  );
}

/** Run a saved script or an ad-hoc command on devices. */
export function RunScriptModal({ deviceIds, scriptId, os, onClose, onDone }: { deviceIds?: string[]; scriptId?: string; os?: string; onClose: () => void; onDone?: () => void }) {
  const scripts = useFetch<any[]>('/scripts');
  const clients = useFetch<any[]>(deviceIds?.length ? null : '/clients');
  const devices = useFetch<any[]>(deviceIds?.length ? null : '/devices');
  const [mode, setMode] = useState<'saved' | 'adhoc'>(scriptId || !deviceIds ? 'saved' : 'adhoc');
  const [sel, setSel] = useState(scriptId || '');
  const [shell, setShell] = useState(defaultShell(os));
  const [body, setBody] = useState('');
  const [args, setArgs] = useState('');
  const [target, setTarget] = useState<'devices' | 'client' | 'all'>('devices');
  const [client, setClient] = useState('');
  const [picked, setPicked] = useState<string[]>(deviceIds || []);
  const [busy, setBusy] = useState(false);
  const fixed = !!deviceIds?.length;
  const visible = (scripts.data || []).filter((x) => !os || !x.platforms?.length || x.platforms.includes(os));

  const run = async () => {
    const payload: any = { args: args.trim() ? args.trim().split(/\s+/) : [] };
    if (mode === 'saved') payload.script_id = sel;
    else Object.assign(payload, { shell, body, timeout_seconds: 300 });
    if (fixed || target === 'devices') payload.device_ids = picked;
    else if (target === 'client') payload.client_id = client;
    else payload.all = true;
    setBusy(true);
    try {
      const job = await api.post('/jobs', payload);
      toast(`Queued on ${job.results.length} device(s)`);
      onClose();
      if (onDone) onDone(); else navigate('/jobs/' + job.id);
    } catch (e: any) {
      toast(e.message, 'error');
    }
    setBusy(false);
  };
  const can = (mode === 'saved' ? !!sel : body.trim().length > 0) && (fixed ? true : target === 'devices' ? picked.length > 0 : target === 'client' ? !!client : true);

  return (
    <Modal title={fixed ? `Run on ${deviceIds!.length === 1 ? 'this device' : deviceIds!.length + ' devices'}` : 'Run script'} onClose={onClose} width={720}
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="primary" icon="play" loading={busy} disabled={!can} onClick={run}>Run now</Button></>}>
      <div class="seg mb">
        <button class={mode === 'saved' ? 'active' : ''} onClick={() => setMode('saved')}>Saved script</button>
        <button class={mode === 'adhoc' ? 'active' : ''} onClick={() => setMode('adhoc')}>Quick command</button>
      </div>
      {mode === 'saved' ? (
        <Field label="Script">
          <select value={sel} onChange={(e) => setSel((e.target as HTMLSelectElement).value)}>
            <option value="">Choose a script…</option>
            {visible.map((x) => <option key={x.id} value={x.id}>{x.category} / {x.name} ({shellLabel(x.shell)})</option>)}
          </select>
        </Field>
      ) : (
        <>
          <Field label="Language"><select value={shell} onChange={(e) => setShell((e.target as HTMLSelectElement).value)}>{SHELLS.map(([v, l]) => <option key={v} value={v}>{l}</option>)}</select></Field>
          <Field label="Command"><textarea class="code" rows={7} spellcheck={false} value={body} onInput={(e) => setBody((e.target as HTMLTextAreaElement).value)} placeholder={shell === 'powershell' ? 'Get-Service | Where-Object Status -eq Running' : 'df -h'} /></Field>
        </>
      )}
      <Field label="Arguments (optional)" hint="Space separated; passed to the script"><input value={args} onInput={(e) => setArgs((e.target as HTMLInputElement).value)} /></Field>
      {!fixed && (
        <>
          <Field label="Run on">
            <div class="seg">
              <button class={target === 'devices' ? 'active' : ''} onClick={() => setTarget('devices')}>Selected devices</button>
              <button class={target === 'client' ? 'active' : ''} onClick={() => setTarget('client')}>All devices of a client</button>
              <button class={target === 'all' ? 'active' : ''} onClick={() => setTarget('all')}>Every device</button>
            </div>
          </Field>
          {target === 'client' && <Field label="Client"><select value={client} onChange={(e) => setClient((e.target as HTMLSelectElement).value)}><option value="">Choose…</option>{clients.data?.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.device_count})</option>)}</select></Field>}
          {target === 'devices' && <DevicePicker devices={devices.data || []} value={picked} onChange={setPicked} />}
        </>
      )}
      <p class="muted small">Offline devices run the script when they reconnect (within 24 hours).</p>
    </Modal>
  );
}

function DevicePicker({ devices, value, onChange }: { devices: any[]; value: string[]; onChange: (v: string[]) => void }) {
  const [s, setS] = useState('');
  const list = devices.filter((d) => !s || (d.hostname + d.display_name + d.client_name).toLowerCase().includes(s.toLowerCase()));
  return (
    <div class="picker">
      <SearchInput value={s} onInput={setS} placeholder="Filter devices" />
      <div class="picker-list">
        {list.map((d) => (
          <label key={d.id} class="check">
            <input type="checkbox" checked={value.includes(d.id)} onChange={() => onChange(value.includes(d.id) ? value.filter((x) => x !== d.id) : [...value, d.id])} />
            <span class={'dot ' + (d.online ? 'on' : 'off')} />{d.display_name || d.hostname} <span class="muted small">{d.client_name}</span>
          </label>
        ))}
      </div>
      <div class="muted small">{value.length} selected</div>
    </div>
  );
}

function Jobs() {
  const { data } = useFetch<any[]>('/jobs', [], 8000);
  if (!data) return <Spinner />;
  if (!data.length) return <Card><Empty icon="history" title="Nothing has run yet" /></Card>;
  return (
    <Card pad={false}>
      <div class="table-wrap">
        <table class="table">
          <thead><tr><th>Job</th><th>Source</th><th>Devices</th><th>Result</th><th>Started</th></tr></thead>
          <tbody>
            {data.map((j) => (
              <tr key={j.id}>
                <td><Link href={'/jobs/' + j.id} class="strong">{j.name}</Link><div class="muted small">{shellLabel(j.shell)} · {j.created_by}</div></td>
                <td><Badge tone={j.source === 'schedule' ? 'accent' : 'neutral'}>{j.source}</Badge></td>
                <td>{j.total}</td>
                <td class="nowrap">
                  {j.success > 0 && <Badge tone="good">{j.success} ok</Badge>} {j.failed > 0 && <Badge tone="critical">{j.failed} failed</Badge>} {j.active > 0 && <Badge tone="accent">{j.active} running</Badge>}
                </td>
                <td class="nowrap muted">{timeAgo(j.created_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

export function JobOutput({ r }: { r: any }) {
  return (
    <div class="job-output">
      {r.stdout ? <pre class="output">{r.stdout}</pre> : null}
      {r.stderr ? <pre class="output err">{r.stderr}</pre> : null}
      {!r.stdout && !r.stderr && <div class="muted small">{['pending', 'running'].includes(r.status) ? 'Waiting for output…' : 'No output.'}</div>}
    </div>
  );
}

export function JobPage({ id }: { id: string }) {
  const { data, error } = useFetch<any>('/jobs/' + id, [], 3000);
  const [open, setOpen] = useState<Set<string>>(new Set());
  if (error) return <div class="page"><ErrorBox msg={error} /></div>;
  if (!data) return <div class="page"><Spinner /></div>;
  const toggle = (rid: string) => { const n = new Set(open); n.has(rid) ? n.delete(rid) : n.add(rid); setOpen(n); };
  const results = data.results || [];
  const count = (st: string[]) => results.filter((r: any) => st.includes(r.status)).length;
  return (
    <div class="page">
      <PageHeader back={<Link href="/scripts?tab=jobs" class="back"><Icon name="chevronLeft" size={16} /> Run history</Link>}
        title={data.name} subtitle={`${shellLabel(data.shell)} · started ${fmtDate(data.created_at)} by ${data.created_by}`} />
      <div class="stats small-stats">
        <div class="stat"><div><div class="stat-label">Devices</div><div class="stat-value">{results.length}</div></div></div>
        <div class="stat stat-good"><div><div class="stat-label">Succeeded</div><div class="stat-value">{count(['success'])}</div></div></div>
        <div class="stat stat-critical"><div><div class="stat-label">Failed</div><div class="stat-value">{count(['failed', 'error', 'timeout', 'expired'])}</div></div></div>
        <div class="stat"><div><div class="stat-label">Pending / running</div><div class="stat-value">{count(['pending', 'running'])}</div></div></div>
      </div>
      <Card title="Results" pad={false} actions={<Button size="sm" variant="ghost" onClick={() => setOpen(open.size ? new Set() : new Set(results.map((r: any) => r.id)))}>{open.size ? 'Collapse all' : 'Expand all'}</Button>}>
        <ul class="list">
          {results.map((r: any) => (
            <li key={r.id} class="col">
              <div class="row clickable" onClick={() => toggle(r.id)}>
                <Icon name={open.has(r.id) ? 'chevronDown' : 'chevronRight'} size={16} />
                <StatusBadge s={r.status} />
                <div class="grow"><Link href={'/devices/' + r.device_id} class="strong">{r.device_name}</Link> <OsBadge os={r.os} /></div>
                {r.exit_code !== null && <span class="mono small muted">exit {r.exit_code}</span>}
                <span class="muted small">{r.finished_at ? timeAgo(r.finished_at) : r.status === 'pending' ? (r.online ? 'queued' : 'waiting for device') : ''}</span>
              </div>
              {open.has(r.id) && <JobOutput r={r} />}
            </li>
          ))}
        </ul>
      </Card>
      <Card title="Script">
        <pre class="output">{data.body}</pre>
      </Card>
    </div>
  );
}

function Schedules() {
  const { data, reload } = useFetch<any[]>('/schedules');
  const [edit, setEdit] = useState<any | null>(null);
  const tech = useCan('technician');
  const runNow = async (s: any) => {
    try {
      const job = await api.post(`/schedules/${s.id}/run`);
      navigate('/jobs/' + job.id);
    } catch (e: any) { toast(e.message, 'error'); }
  };
  return (
    <>
      <div class="toolbar"><div class="grow" />{tech && <Button variant="primary" icon="plus" onClick={() => setEdit({ enabled: true, cron: '0 3 * * *', target_type: 'all', target_ids: [] })}>New schedule</Button>}</div>
      {!data ? <Spinner /> : !data.length ? <Card><Empty icon="calendar" title="No scheduled tasks">Run maintenance scripts automatically, e.g. every night at 3am.</Empty></Card> : (
        <Card pad={false}>
          <table class="table">
            <thead><tr><th>Name</th><th>Script</th><th>When</th><th>Targets</th><th>Last run</th><th>Next run</th><th /></tr></thead>
            <tbody>
              {data.map((s) => (
                <tr key={s.id}>
                  <td><button class="linklike strong" onClick={() => setEdit(s)}>{s.name}</button> {!s.enabled && <Badge>disabled</Badge>}</td>
                  <td>{s.script_name}</td>
                  <td class="mono small">{s.cron}</td>
                  <td>{s.target_type === 'all' ? 'All devices' : s.target_type === 'client' ? `${s.target_ids.length} client(s)` : `${s.target_ids.length} device(s)`}</td>
                  <td class="muted">{timeAgo(s.last_run)}</td>
                  <td class="muted">{s.enabled ? fmtDate(s.next_run) : '—'}</td>
                  <td class="right">{tech && <Button size="sm" icon="play" onClick={() => runNow(s)}>Run now</Button>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
      {edit && <ScheduleEditor s={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload(true); }} />}
    </>
  );
}

const CRON_PRESETS: [string, string][] = [['0 3 * * *', 'Daily at 03:00'], ['0 */6 * * *', 'Every 6 hours'], ['0 * * * *', 'Hourly'], ['0 2 * * 0', 'Weekly (Sun 02:00)'], ['0 4 1 * *', 'Monthly (1st, 04:00)']];

function ScheduleEditor({ s, onClose, onSaved }: { s: any; onClose: () => void; onSaved: () => void }) {
  const scripts = useFetch<any[]>('/scripts');
  const clients = useFetch<any[]>('/clients');
  const devices = useFetch<any[]>('/devices');
  const [v, setV] = useState({ ...s });
  const set = (k: string, val: any) => setV({ ...v, [k]: val });
  const save = async () => {
    if (await attempt(() => (v.id ? api.put('/schedules/' + v.id, v) : api.post('/schedules', v)), 'Schedule saved')) onSaved();
  };
  const del = async () => {
    if (!(await confirmDialog({ title: 'Delete schedule?', danger: true, confirm: 'Delete' }))) return;
    if (await attempt(() => api.del('/schedules/' + v.id), 'Schedule deleted')) onSaved();
  };
  return (
    <Modal title={v.id ? 'Edit schedule' : 'New schedule'} onClose={onClose} width={680}
      footer={<>{v.id && <Button variant="danger" icon="trash" onClick={del}>Delete</Button>}<div class="grow" /><Button onClick={onClose}>Cancel</Button><Button variant="primary" onClick={save}>Save</Button></>}>
      <Field label="Name"><input value={v.name || ''} onInput={(e) => set('name', (e.target as HTMLInputElement).value)} /></Field>
      <Field label="Script"><select value={v.script_id || ''} onChange={(e) => set('script_id', (e.target as HTMLSelectElement).value)}><option value="">Choose…</option>{scripts.data?.map((x) => <option key={x.id} value={x.id}>{x.category} / {x.name}</option>)}</select></Field>
      <Field label="Schedule (cron, server time)" hint={<>Presets: {CRON_PRESETS.map(([c, l]) => <button key={c} class="chip" onClick={() => set('cron', c)}>{l}</button>)}</>}>
        <input class="mono" value={v.cron} onInput={(e) => set('cron', (e.target as HTMLInputElement).value)} />
      </Field>
      <Field label="Targets">
        <div class="seg">
          {[['all', 'All devices'], ['client', 'Clients'], ['devices', 'Specific devices']].map(([k, l]) => <button key={k} class={v.target_type === k ? 'active' : ''} onClick={() => setV({ ...v, target_type: k, target_ids: [] })}>{l}</button>)}
        </div>
      </Field>
      {v.target_type === 'client' && (
        <div class="checks col">{clients.data?.map((c) => <label key={c.id} class="check"><input type="checkbox" checked={v.target_ids.includes(c.id)} onChange={() => set('target_ids', v.target_ids.includes(c.id) ? v.target_ids.filter((x: string) => x !== c.id) : [...v.target_ids, c.id])} /> {c.name}</label>)}</div>
      )}
      {v.target_type === 'devices' && <DevicePicker devices={devices.data || []} value={v.target_ids} onChange={(ids) => set('target_ids', ids)} />}
      <label class="check mt"><input type="checkbox" checked={v.enabled} onChange={() => set('enabled', !v.enabled)} /> Enabled</label>
    </Modal>
  );
}
