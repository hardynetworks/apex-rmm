import { useState } from 'preact/hooks';
import { api } from '../api';
import { Icon } from '../icons';
import { Link, navigate, useQuery } from '../router';
import {
  Badge, Button, Card, Empty, ErrorBox, Field, Kv, Modal, PageHeader, Priority, SearchInput, Severity, Spinner, StatusBadge,
  attempt, confirmDialog, fmtDate, timeAgo, toast, useFetch,
} from '../ui';
import { useCan, useUser } from '../user';

const STATUSES: [string, string][] = [['open', 'Open'], ['in_progress', 'In progress'], ['waiting', 'Waiting on customer'], ['resolved', 'Resolved'], ['closed', 'Closed']];
const PRIORITIES: [string, string][] = [['low', 'Low'], ['medium', 'Medium'], ['high', 'High'], ['urgent', 'Urgent']];

export function Tickets() {
  const q = useQuery();
  const status = q.get('status') || 'active';
  const assignee = q.get('assignee') || '';
  const [search, setSearch] = useState('');
  const [newOpen, setNewOpen] = useState(false);
  const tech = useCan('technician');
  const params = new URLSearchParams({ status });
  if (assignee) params.set('assignee', assignee);
  if (search) params.set('q', search);
  const { data, error } = useFetch<any[]>('/tickets?' + params, [], 20000);
  const setParam = (k: string, v: string) => {
    const p = new URLSearchParams(location.search);
    v ? p.set(k, v) : p.delete(k);
    navigate('/tickets?' + p, true);
  };
  const views: [string, string, string][] = [['active', '', 'All open'], ['active', 'me', 'Assigned to me'], ['active', 'none', 'Unassigned'], ['resolved', '', 'Resolved'], ['all', '', 'Everything']];
  return (
    <div class="page">
      <PageHeader title="Tickets" subtitle="Helpdesk requests linked to clients and devices."
        actions={tech && <Button variant="primary" icon="plus" onClick={() => setNewOpen(true)}>New ticket</Button>} />
      <div class="toolbar">
        <div class="seg">
          {views.map(([s, a, l]) => <button key={l} class={status === s && assignee === a ? 'active' : ''} onClick={() => navigate(`/tickets?status=${s}${a ? '&assignee=' + a : ''}`, true)}>{l}</button>)}
        </div>
        <SearchInput value={search} onInput={setSearch} placeholder="Search or #number" />
      </div>
      {error && <ErrorBox msg={error} />}
      <Card pad={false}>
        {!data ? <Spinner /> : !data.length ? <Empty icon="ticket" title="No tickets here" /> : (
          <div class="table-wrap">
            <table class="table">
              <thead><tr><th>Ticket</th><th>Priority</th><th>Status</th><th class="hide-sm">Client / device</th><th class="hide-md">Assignee</th><th>Updated</th></tr></thead>
              <tbody>
                {data.map((t) => (
                  <tr key={t.id}>
                    <td><Link href={'/tickets/' + t.id} class="strong">#{t.number} {t.title}</Link>
                      <div class="muted small">{t.requester_name || t.created_by}{t.comment_count > 0 && <> · <Icon name="send" size={11} /> {t.comment_count}</>}{t.time_minutes > 0 && <> · <Icon name="clock" size={11} /> {(t.time_minutes / 60).toFixed(1)}h</>}{t.source === 'alert' && <> · <Badge tone="warning">alert</Badge></>}</div></td>
                    <td><Priority p={t.priority} /></td>
                    <td><StatusBadge s={t.status} /></td>
                    <td class="hide-sm">{t.client_name || '—'}<div class="muted small">{t.device_name}</div></td>
                    <td class="hide-md">{t.assignee_name || <span class="muted">Unassigned</span>}</td>
                    <td class="nowrap muted">{timeAgo(t.updated_at)}{t.due_at && <div class={'small ' + (new Date(t.due_at) < new Date() && !['resolved', 'closed'].includes(t.status) ? 'text-crit' : '')}>due {fmtDate(t.due_at)}</div>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
      {newOpen && <NewTicketModal onClose={() => setNewOpen(false)} />}
    </div>
  );
}

export function NewTicketModal({ onClose, deviceId, clientId }: { onClose: () => void; deviceId?: string; clientId?: string }) {
  const clients = useFetch<any[]>('/clients');
  const devices = useFetch<any[]>('/devices');
  const users = useFetch<any[]>('/users');
  const me = useUser();
  const [v, setV] = useState<any>({ title: '', description: '', priority: 'medium', client_id: clientId || '', device_id: deviceId || '', assignee_id: me.id, requester_name: '', requester_email: '', due_at: '' });
  const set = (k: string, val: any) => setV({ ...v, [k]: val });
  const [busy, setBusy] = useState(false);
  const create = async () => {
    setBusy(true);
    try {
      const t = await api.post('/tickets', v);
      toast(`Ticket #${t.number} created`);
      onClose();
      navigate('/tickets/' + t.id);
    } catch (e: any) { toast(e.message, 'error'); }
    setBusy(false);
  };
  const devs = (devices.data || []).filter((d) => !v.client_id || d.client_id === v.client_id);
  return (
    <Modal title="New ticket" onClose={onClose} width={680}
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="primary" loading={busy} disabled={!v.title.trim()} onClick={create}>Create ticket</Button></>}>
      <Field label="Subject"><input value={v.title} autoFocus onInput={(e) => set('title', (e.target as HTMLInputElement).value)} /></Field>
      <Field label="Description"><textarea rows={5} value={v.description} onInput={(e) => set('description', (e.target as HTMLTextAreaElement).value)} /></Field>
      <div class="row-fields">
        <Field label="Client"><select value={v.client_id} onChange={(e) => setV({ ...v, client_id: (e.target as HTMLSelectElement).value, device_id: '' })}><option value="">—</option>{clients.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
        <Field label="Device"><select value={v.device_id} onChange={(e) => set('device_id', (e.target as HTMLSelectElement).value)}><option value="">—</option>{devs.map((d) => <option key={d.id} value={d.id}>{d.display_name || d.hostname}</option>)}</select></Field>
      </div>
      <div class="row-fields">
        <Field label="Priority"><select value={v.priority} onChange={(e) => set('priority', (e.target as HTMLSelectElement).value)}>{PRIORITIES.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></Field>
        <Field label="Assignee"><select value={v.assignee_id} onChange={(e) => set('assignee_id', (e.target as HTMLSelectElement).value)}><option value="">Unassigned</option>{users.data?.filter((u) => u.role !== 'viewer').map((u) => <option key={u.id} value={u.id}>{u.name || u.email}</option>)}</select></Field>
        <Field label="Due"><input type="datetime-local" value={v.due_at} onInput={(e) => set('due_at', (e.target as HTMLInputElement).value)} /></Field>
      </div>
      <div class="row-fields">
        <Field label="Requester name"><input value={v.requester_name} onInput={(e) => set('requester_name', (e.target as HTMLInputElement).value)} /></Field>
        <Field label="Requester email"><input type="email" value={v.requester_email} onInput={(e) => set('requester_email', (e.target as HTMLInputElement).value)} /></Field>
      </div>
    </Modal>
  );
}

export function TicketPage({ id }: { id: string }) {
  const { data: t, error, setData } = useFetch<any>('/tickets/' + id);
  const users = useFetch<any[]>('/users');
  const clients = useFetch<any[]>('/clients');
  const tech = useCan('technician');
  const admin = useCan('admin');
  const [body, setBody] = useState('');
  const [internal, setInternal] = useState(false);
  const [mins, setMins] = useState(0);
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState({ title: '', description: '' });

  if (error) return <div class="page"><ErrorBox msg={error} /></div>;
  if (!t) return <div class="page"><Spinner /></div>;

  const patch = (b: any) => attempt(async () => setData(await api.patch('/tickets/' + id, b)));
  const comment = async (status?: string) => {
    setBusy(true);
    await attempt(async () => {
      setData(await api.post(`/tickets/${id}/comments`, { body, internal, time_minutes: mins, status: status || '' }));
      setBody(''); setMins(0);
    });
    setBusy(false);
  };
  const del = async () => {
    if (!(await confirmDialog({ title: `Delete ticket #${t.number}?`, danger: true, confirm: 'Delete' }))) return;
    if (await attempt(() => api.del('/tickets/' + id), 'Ticket deleted')) navigate('/tickets');
  };
  const totalTime = (t.comments || []).reduce((a: number, c: any) => a + (c.time_minutes || 0), 0);

  return (
    <div class="page">
      <PageHeader back={<Link href="/tickets" class="back"><Icon name="chevronLeft" size={16} /> Tickets</Link>}
        title={editing ? <input class="title-input" value={draft.title} onInput={(e) => setDraft({ ...draft, title: (e.target as HTMLInputElement).value })} /> : <>#{t.number} {t.title}</>}
        subtitle={<span class="meta-row"><StatusBadge s={t.status} /> <Priority p={t.priority} /> <span>opened {fmtDate(t.created_at)} by {t.created_by}</span></span>}
        actions={<>
          {tech && !editing && <Button icon="edit" onClick={() => { setDraft({ title: t.title, description: t.description }); setEditing(true); }}>Edit</Button>}
          {editing && <><Button onClick={() => setEditing(false)}>Cancel</Button><Button variant="primary" onClick={async () => { await patch(draft); setEditing(false); }}>Save</Button></>}
          {admin && <Button variant="ghost" icon="trash" onClick={del} title="Delete ticket" />}
        </>} />
      <div class="ticket-layout">
        <div class="stack">
          <Card title="Description">
            {editing ? <textarea rows={8} value={draft.description} onInput={(e) => setDraft({ ...draft, description: (e.target as HTMLTextAreaElement).value })} /> : <p class="prewrap">{t.description || <span class="muted">No description.</span>}</p>}
          </Card>
          <Card title="Activity" pad={false}>
            <ul class="timeline">
              {(t.comments || []).map((c: any) => c.kind === 'event' ? (
                <li key={c.id} class="event"><Icon name="info" size={14} /> <span>{c.body}</span> <span class="muted small">· {c.author_name} · {timeAgo(c.created_at)}</span></li>
              ) : (
                <li key={c.id} class={'comment ' + (c.internal ? 'internal' : '')}>
                  <div class="avatar sm">{(c.author_name || '?').slice(0, 1).toUpperCase()}</div>
                  <div class="grow">
                    <div class="comment-head"><strong>{c.author_name}</strong> <span class="muted small">{fmtDate(c.created_at)}</span>{c.internal && <Badge tone="warning">internal note</Badge>}{c.time_minutes > 0 && <Badge tone="neutral"><Icon name="clock" size={11} /> {c.time_minutes} min</Badge>}</div>
                    {c.body && <div class="prewrap">{c.body}</div>}
                  </div>
                </li>
              ))}
            </ul>
            {tech && (
              <div class="composer">
                <textarea rows={4} placeholder="Write a reply or internal note…" value={body} onInput={(e) => setBody((e.target as HTMLTextAreaElement).value)} />
                <div class="composer-bar">
                  <label class="check"><input type="checkbox" checked={internal} onChange={() => setInternal(!internal)} /> Internal note</label>
                  <label class="check">Time spent <input type="number" min={0} step={5} class="mini" value={mins} onInput={(e) => setMins(+(e.target as HTMLInputElement).value)} /> min</label>
                  <div class="grow" />
                  {!['resolved', 'closed'].includes(t.status) && <Button disabled={!body.trim() && !mins} loading={busy} onClick={() => comment('resolved')}>Add &amp; resolve</Button>}
                  <Button variant="primary" icon="send" disabled={!body.trim() && !mins} loading={busy} onClick={() => comment()}>Add</Button>
                </div>
              </div>
            )}
          </Card>
        </div>
        <div class="stack">
          <Card title="Properties">
            <div class="props">
              <Field label="Status"><select disabled={!tech} value={t.status} onChange={(e) => patch({ status: (e.target as HTMLSelectElement).value })}>{STATUSES.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></Field>
              <Field label="Priority"><select disabled={!tech} value={t.priority} onChange={(e) => patch({ priority: (e.target as HTMLSelectElement).value })}>{PRIORITIES.map(([k, l]) => <option key={k} value={k}>{l}</option>)}</select></Field>
              <Field label="Assignee"><select disabled={!tech} value={t.assignee_id || ''} onChange={(e) => patch({ assignee_id: (e.target as HTMLSelectElement).value })}><option value="">Unassigned</option>{users.data?.filter((u) => u.role !== 'viewer').map((u) => <option key={u.id} value={u.id}>{u.name || u.email}</option>)}</select></Field>
              <Field label="Client"><select disabled={!tech} value={t.client_id || ''} onChange={(e) => patch({ client_id: (e.target as HTMLSelectElement).value })}><option value="">—</option>{clients.data?.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}</select></Field>
              <Field label="Due"><input disabled={!tech} type="datetime-local" value={t.due_at ? toLocalInput(t.due_at) : ''} onChange={(e) => patch({ due_at: (e.target as HTMLInputElement).value })} /></Field>
            </div>
          </Card>
          <Card title="Details">
            <Kv items={[
              ['Device', t.device_id ? <Link href={'/devices/' + t.device_id} class="link">{t.device_name}</Link> : '—'],
              ['Requester', t.requester_name ? <>{t.requester_name}{t.requester_email && <div class="muted small">{t.requester_email}</div>}</> : '—'],
              ['Source', t.source],
              ['Time logged', totalTime ? `${(totalTime / 60).toFixed(2)} h` : '—'],
              ['Resolved', t.resolved_at ? fmtDate(t.resolved_at) : '—'],
            ]} />
            {t.device_id && tech && (
              <div class="stack mt">
                <Button icon="pointer" disabled={!t.device_online} onClick={() => window.open(`/devices/${t.device_id}/remote`, 'apex-rd-' + t.device_id, 'popup,width=1400,height=900')}>Remote control</Button>
                <Link href={`/devices/${t.device_id}?tab=terminal`} class="btn btn-default btn-md"><Icon name="terminal" /><span>Open terminal</span></Link>
              </div>
            )}
          </Card>
          {t.alerts?.length > 0 && (
            <Card title="Linked alerts" pad={false}>
              <ul class="list">{t.alerts.map((a: any) => <li key={a.id}><Severity s={a.severity} /><div class="grow small">{a.title}</div><StatusBadge s={a.status} /></li>)}</ul>
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}

function toLocalInput(iso: string) {
  const d = new Date(iso);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
