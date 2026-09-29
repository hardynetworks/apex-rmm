import { useState } from 'preact/hooks';
import { api } from '../api';
import { Link, navigate, useQuery } from '../router';
import { Badge, Button, Card, Empty, Field, Modal, OsBadge, PageHeader, Severity, Spinner, StatusBadge, Tabs, attempt, confirmDialog, timeAgo, useFetch } from '../ui';
import { useCan } from '../user';

const metricLabel: Record<string, string> = { cpu: 'CPU usage', mem: 'Memory usage', disk: 'Disk usage', offline: 'Device offline' };

export function Alerts() {
  const q = useQuery();
  const tab = q.get('tab') || 'active';
  return (
    <div class="page">
      <PageHeader title="Alerts" subtitle="Threshold monitoring across all devices." />
      <Tabs tabs={[{ id: 'active', label: 'Active', icon: 'bell' }, { id: 'resolved', label: 'Resolved', icon: 'check' }, { id: 'policies', label: 'Policies', icon: 'shield' }]}
        active={tab} onChange={(t) => navigate('/alerts?tab=' + t, true)} />
      {tab === 'policies' ? <Policies /> : <AlertList status={tab === 'resolved' ? 'resolved' : ''} />}
    </div>
  );
}

function AlertList({ status }: { status: string }) {
  const { data, reload } = useFetch<any[]>('/alerts' + (status ? '?status=' + status : ''), [], 15000);
  const tech = useCan('technician');
  if (!data) return <Spinner />;
  if (!data.length) return <Card><Empty icon="check" title={status ? 'No resolved alerts' : 'All clear'}>{!status && 'Nothing needs your attention right now.'}</Empty></Card>;
  const act = async (id: string, what: string) => { if (await attempt(() => api.post(`/alerts/${id}/${what}`))) reload(true); };
  const ticket = async (id: string) => {
    try { const r = await api.post(`/alerts/${id}/ticket`); navigate('/tickets/' + r.ticket_id); } catch (e: any) { attempt(() => Promise.reject(e)); }
  };
  return (
    <Card pad={false}>
      <div class="table-wrap">
        <table class="table">
          <thead><tr><th>Severity</th><th>Alert</th><th class="hide-sm">Device</th><th>Status</th><th>Raised</th><th /></tr></thead>
          <tbody>
            {data.map((a) => (
              <tr key={a.id}>
                <td><Severity s={a.severity} /></td>
                <td><span class="strong">{a.title}</span><div class="muted small">{a.message}</div></td>
                <td class="hide-sm"><Link href={'/devices/' + a.device_id} class="strong">{a.device_name}</Link><div><OsBadge os={a.os} /> <span class="muted small">{a.client_name}</span></div></td>
                <td><StatusBadge s={a.status} />{a.acknowledged_by && <div class="muted small">by {a.acknowledged_by}</div>}</td>
                <td class="nowrap muted">{timeAgo(a.created_at)}{a.resolved_at && <div class="small">resolved {timeAgo(a.resolved_at)}</div>}</td>
                <td class="right nowrap">
                  {a.ticket_id ? <Link href={'/tickets/' + a.ticket_id} class="link small">Ticket #{a.ticket_number}</Link> : tech && a.status !== 'resolved' && <Button size="sm" variant="ghost" onClick={() => ticket(a.id)}>Create ticket</Button>}
                  {tech && a.status === 'open' && <Button size="sm" variant="ghost" onClick={() => act(a.id, 'ack')}>Acknowledge</Button>}
                  {tech && a.status !== 'resolved' && <Button size="sm" variant="ghost" onClick={() => act(a.id, 'resolve')}>Resolve</Button>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

function Policies() {
  const { data, reload } = useFetch<any[]>('/alert-policies');
  const [edit, setEdit] = useState<any | null>(null);
  const admin = useCan('admin');
  return (
    <>
      <div class="toolbar"><p class="muted grow">Policies are evaluated every minute. Devices in maintenance mode are skipped.</p>{admin && <Button variant="primary" icon="plus" onClick={() => setEdit({ enabled: true, metric: 'cpu', operator: '>', threshold: 90, duration_minutes: 10, severity: 'warning', notify: true, create_ticket: false })}>New policy</Button>}</div>
      {!data ? <Spinner /> : (
        <Card pad={false}>
          <table class="table">
            <thead><tr><th>Policy</th><th>Condition</th><th>Severity</th><th class="hide-sm">Scope</th><th class="hide-sm">Actions</th><th>Open</th></tr></thead>
            <tbody>
              {data.map((p) => (
                <tr key={p.id}>
                  <td>{admin ? <button class="linklike strong" onClick={() => setEdit(p)}>{p.name}</button> : <span class="strong">{p.name}</span>} {!p.enabled && <Badge>disabled</Badge>}</td>
                  <td>{p.metric === 'offline' ? `Offline for ${p.duration_minutes} min` : `${metricLabel[p.metric]} ${p.operator} ${p.threshold}% for ${p.duration_minutes} min`}</td>
                  <td><Severity s={p.severity} /></td>
                  <td class="hide-sm">{p.client_name || 'All clients'}</td>
                  <td class="hide-sm small">{[p.notify && 'Notify', p.create_ticket && 'Create ticket'].filter(Boolean).join(', ') || '—'}</td>
                  <td>{p.open_alerts}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
      {edit && <PolicyEditor p={edit} onClose={() => setEdit(null)} onSaved={() => { setEdit(null); reload(true); }} />}
    </>
  );
}

function PolicyEditor({ p, onClose, onSaved }: { p: any; onClose: () => void; onSaved: () => void }) {
  const clients = useFetch<any[]>('/clients');
  const [v, setV] = useState({ ...p });
  const set = (k: string, val: any) => setV({ ...v, [k]: val });
  const save = async () => { if (await attempt(() => (v.id ? api.put('/alert-policies/' + v.id, v) : api.post('/alert-policies', v)), 'Policy saved')) onSaved(); };
  const del = async () => {
    if (!(await confirmDialog({ title: `Delete "${v.name}"?`, danger: true, confirm: 'Delete' }))) return;
    if (await attempt(() => api.del('/alert-policies/' + v.id), 'Policy deleted')) onSaved();
  };
  return (
    <Modal title={v.id ? 'Edit alert policy' : 'New alert policy'} onClose={onClose}
      footer={<>{v.id && <Button variant="danger" icon="trash" onClick={del}>Delete</Button>}<div class="grow" /><Button onClick={onClose}>Cancel</Button><Button variant="primary" onClick={save}>Save</Button></>}>
      <Field label="Name"><input value={v.name || ''} onInput={(e) => set('name', (e.target as HTMLInputElement).value)} /></Field>
      <div class="row-fields">
        <Field label="Metric"><select value={v.metric} onChange={(e) => set('metric', (e.target as HTMLSelectElement).value)}>{Object.entries(metricLabel).map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></Field>
        {v.metric !== 'offline' && <Field label="Operator"><select value={v.operator} onChange={(e) => set('operator', (e.target as HTMLSelectElement).value)}><option value=">">above</option><option value="<">below</option></select></Field>}
        {v.metric !== 'offline' && <Field label="Threshold (%)"><input type="number" value={v.threshold} onInput={(e) => set('threshold', +(e.target as HTMLInputElement).value)} /></Field>}
        <Field label={v.metric === 'offline' ? 'Offline for (min)' : 'Sustained for (min)'}><input type="number" min={1} value={v.duration_minutes} onInput={(e) => set('duration_minutes', +(e.target as HTMLInputElement).value)} /></Field>
      </div>
      <div class="row-fields">
        <Field label="Severity"><select value={v.severity} onChange={(e) => set('severity', (e.target as HTMLSelectElement).value)}><option value="info">Info</option><option value="warning">Warning</option><option value="critical">Critical</option></select></Field>
        <Field label="Applies to"><select value={v.client_id || ''} onChange={(e) => set('client_id', (e.target as HTMLSelectElement).value)}><option value="">All clients</option>{clients.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
      </div>
      <div class="checks col">
        <label class="check"><input type="checkbox" checked={v.enabled} onChange={() => set('enabled', !v.enabled)} /> Enabled</label>
        <label class="check"><input type="checkbox" checked={v.notify} onChange={() => set('notify', !v.notify)} /> Send webhook notification (configure in Settings)</label>
        <label class="check"><input type="checkbox" checked={v.create_ticket} onChange={() => set('create_ticket', !v.create_ticket)} /> Automatically open a ticket</label>
      </div>
    </Modal>
  );
}
