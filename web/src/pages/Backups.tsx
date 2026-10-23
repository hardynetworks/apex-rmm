import { useEffect, useState } from 'preact/hooks';
import { api } from '../api';
import { Badge, Button, Card, Empty, ErrorBox, Field, Kv, Modal, Spinner, attempt, confirmDialog, fmtBytes, fmtDate, timeAgo, toast, useFetch } from '../ui';

export type S3 = { endpoint: string; region: string; bucket: string; prefix: string; access_key: string; secret_key: string; path_style: boolean };
export const emptyS3: S3 = { endpoint: '', region: '', bucket: '', prefix: 'apex', access_key: '', secret_key: '', path_style: false };

const PROVIDERS: { name: string; endpoint: string; region: string; path: boolean }[] = [
  { name: 'AWS S3', endpoint: 'https://s3.us-east-1.amazonaws.com', region: 'us-east-1', path: false },
  { name: 'Cloudflare R2', endpoint: 'https://<account-id>.r2.cloudflarestorage.com', region: 'auto', path: true },
  { name: 'Backblaze B2', endpoint: 'https://s3.us-west-004.backblazeb2.com', region: 'us-west-004', path: false },
  { name: 'Wasabi', endpoint: 'https://s3.us-east-1.wasabisys.com', region: 'us-east-1', path: false },
  { name: 'MinIO', endpoint: 'https://minio.example.com:9000', region: '', path: true },
];

/** Bucket fields, shared by Settings → Backups and the setup wizard's restore. */
export function S3Fields({ v, onChange, secretSet }: { v: S3; onChange: (v: S3) => void; secretSet?: boolean }) {
  const set = (k: keyof S3, val: any) => onChange({ ...v, [k]: val });
  const inp = (k: keyof S3, props: any = {}) => <input value={v[k] as string} onInput={(e) => set(k, (e.target as HTMLInputElement).value)} {...props} />;
  return (
    <div class="stack-sm">
      <div class="chips">
        <span class="muted small">Fill in for:</span>
        {PROVIDERS.map((p) => (
          <button type="button" key={p.name} class="chip" onClick={() => onChange({ ...v, endpoint: p.endpoint, region: p.region, path_style: p.path })}>{p.name}</button>
        ))}
      </div>
      <Field label="Endpoint" hint="The S3 API address of your provider, without the bucket name.">{inp('endpoint', { placeholder: 'https://s3.us-east-1.amazonaws.com', autoComplete: 'off' })}</Field>
      <div class="row-fields">
        <Field label="Bucket">{inp('bucket', { placeholder: 'my-apex-backups', autoComplete: 'off' })}</Field>
        <Field label="Region" hint="Optional for most providers">{inp('region', { placeholder: 'us-east-1', autoComplete: 'off' })}</Field>
      </div>
      <Field label="Folder in the bucket" hint="Optional. Backups are stored as <folder>/apex-backup-<date>.tar.gz.age">{inp('prefix', { placeholder: 'apex', autoComplete: 'off' })}</Field>
      <div class="row-fields">
        <Field label="Access key ID">{inp('access_key', { autoComplete: 'off', class: 'mono' })}</Field>
        <Field label="Secret access key" hint={secretSet ? 'Saved. Leave empty to keep it.' : 'Stored encrypted.'}>
          {inp('secret_key', { type: 'password', autoComplete: 'new-password', placeholder: secretSet ? '••••••••••••' : '' })}
        </Field>
      </div>
      <label class="check"><input type="checkbox" checked={v.path_style} onChange={() => set('path_style', !v.path_style)} /> Path-style URLs (needed for MinIO, R2 and some self-hosted services)</label>
    </div>
  );
}

function until(t: string): string {
  const m = Math.round((new Date(t).getTime() - Date.now()) / 60000);
  if (m <= 0) return 'due now';
  if (m < 60) return `in ${m} min`;
  const h = Math.floor(m / 60);
  return h < 24 ? `in ${h} h ${m % 60} min` : `in ${Math.round(h / 24)} days`;
}

/** Settings → Backups */
export function BackupSettings() {
  const { data, setData, reload } = useFetch<any>('/backup', [], 0);
  const [s3, setS3] = useState<S3>(emptyS3);
  const [pass, setPass] = useState('');
  const [pass2, setPass2] = useState('');
  const [sched, setSched] = useState({ enabled: true, time: '02:30', keep: 14 });
  const [busy, setBusy] = useState(false);
  const [testing, setTesting] = useState(false);
  const [test, setTest] = useState<any>(null);
  const [listKey, setListKey] = useState(0);
  const [restoring, setRestoring] = useState<any>(null);
  const [progress, setProgress] = useState(false);

  useEffect(() => {
    if (!data) return;
    setS3({ ...emptyS3, ...data.s3, secret_key: '' });
    setSched({ enabled: data.configured ? data.enabled : true, time: data.time, keep: data.keep });
  }, [data?.s3?.endpoint, data?.s3?.bucket, data?.s3?.prefix, data?.enabled, data?.time, data?.keep]);

  // follow a running backup
  const running = data?.running;
  useEffect(() => {
    if (!running) return;
    const t = setInterval(async () => {
      try {
        const d = await api.get('/backup');
        setData(d);
        if (!d.running) {
          setListKey((k) => k + 1);
          if (d.state?.last_ok) toast('Backup uploaded');
          else if (d.state?.last_error) toast('Backup failed: ' + d.state.last_error, 'error');
        }
      } catch { /* ignore */ }
    }, 1000);
    return () => clearInterval(t);
  }, [running?.op]);

  if (!data) return <Spinner />;
  const st = data.state || {};

  const save = async () => {
    if (pass && pass !== pass2) return toast('The passphrases do not match', 'error');
    setBusy(true);
    try {
      const d = await api.put('/backup/settings', { s3, passphrase: pass, ...sched, keep: Number(sched.keep) });
      setData(d);
      setPass('');
      setPass2('');
      setListKey((k) => k + 1);
      toast('Backup settings saved');
    } catch (e: any) {
      toast(e.message, 'error');
    }
    setBusy(false);
  };
  const runTest = async () => {
    setTesting(true);
    setTest(null);
    try { setTest(await api.post('/backup/test', { s3 })); } catch (e: any) { setTest({ ok: false, error: e.message }); }
    setTesting(false);
  };
  const runNow = async () => {
    if (await attempt(() => api.post('/backup/run'))) reload(true);
  };

  return (
    <div class="stack">
      <div class="grid-2">
        <Card title="Bucket" icon="archive" actions={data.configured ? <Badge tone="good">set up</Badge> : <Badge>not set up</Badge>}>
          <S3Fields v={s3} onChange={setS3} secretSet={data.secret_key_set} />
          <div class="inline">
            <Button onClick={runTest} loading={testing} disabled={!s3.endpoint || !s3.bucket}>Test connection</Button>
            {test && (test.ok ? <span class="small text-good">Bucket reachable: write, read and delete work ✓</span> : <span class="small text-crit">{test.error}</span>)}
          </div>
        </Card>
        <div class="stack">
          <Card title="Encryption">
            <p class="small muted">Backups are encrypted with this passphrase before they leave the server. You need it to restore, including on a new server.
              <strong> Keep a copy somewhere safe: without it, backups can't be restored.</strong></p>
            <div class="row-fields">
              <Field label={data.passphrase_set ? 'New passphrase' : 'Passphrase'} hint={data.passphrase_set ? 'A passphrase is saved. Leave empty to keep it.' : 'At least 12 characters'}>
                <input type="password" autoComplete="new-password" value={pass} onInput={(e) => setPass((e.target as HTMLInputElement).value)} placeholder={data.passphrase_set ? '••••••••••••' : ''} />
              </Field>
              <Field label="Repeat passphrase"><input type="password" autoComplete="new-password" value={pass2} onInput={(e) => setPass2((e.target as HTMLInputElement).value)} /></Field>
            </div>
            {data.passphrase_set && pass && <p class="small text-warn">Older backups still need the passphrase they were made with.</p>}
          </Card>
          <Card title="Schedule">
            <label class="check"><input type="checkbox" checked={sched.enabled} onChange={() => setSched({ ...sched, enabled: !sched.enabled })} /> Back up every day</label>
            <div class="row-fields">
              <Field label="At" hint={`Server time, ${data.server_tz}`}>
                <input type="time" value={sched.time} disabled={!sched.enabled} onInput={(e) => setSched({ ...sched, time: (e.target as HTMLInputElement).value })} />
              </Field>
              <Field label="Keep the newest" hint="Older backups are deleted from the bucket">
                <div class="inline" style="margin-top:0"><input type="number" min={1} max={1000} class="w-num" value={sched.keep} onInput={(e) => setSched({ ...sched, keep: Number((e.target as HTMLInputElement).value) })} /><span class="muted small">backups</span></div>
              </Field>
            </div>
          </Card>
        </div>
      </div>
      <div class="form-actions">
        <Button variant="primary" onClick={save} loading={busy}>Save backup settings</Button>
      </div>

      <Card title="Backups" icon="history" actions={<>
        <Button icon="refresh" size="sm" variant="ghost" onClick={() => { reload(true); setListKey((k) => k + 1); }} title="Refresh" />
        <Button icon="upload" variant="primary" size="sm" onClick={runNow} disabled={!data.configured || !!running} loading={running?.op === 'backup'}>Back up now</Button>
      </>}>
        {running && <div class="backup-running"><span class="spinner sm" /> {running.op === 'restore' ? 'Restoring' : 'Backing up'}: {running.phase}</div>}
        <Kv items={[
          ['Last backup', st.last_run ? <span>{st.last_ok ? <Badge tone="good">OK</Badge> : <Badge tone="critical">failed</Badge>} {fmtDate(st.last_run)} <span class="muted">({st.last_trigger}{st.last_ok ? `, ${fmtBytes(st.last_size)}, ${Math.max(1, Math.round(st.last_seconds))} s` : ''})</span></span> : <span class="muted">never</span>],
          ...(st.last_error ? [['Error', <span class="text-crit">{st.last_error}</span>] as [string, any]] : []),
          ['Next scheduled', data.next_run ? `${fmtDate(data.next_run)} (${until(data.next_run)})` : <span class="muted">{data.configured ? 'not scheduled' : 'save the settings first'}</span>],
          ...(st.last_restore ? [['Last restore', <span>{st.last_restore.name} {st.last_restore.created ? `(made ${fmtDate(st.last_restore.created)})` : ''} · {fmtDate(st.last_restore.at)} by {st.last_restore.by}</span>] as [string, any]] : []),
        ]} />
        {data.configured && <BackupList key={listKey} onRestore={setRestoring} disabled={!!running} />}
      </Card>
      {restoring && <RestoreModal backup={restoring} onClose={() => setRestoring(null)} onStarted={() => { setRestoring(null); setProgress(true); }} />}
      {progress && <RestoreProgress statusPath="/backup/restore" onClose={() => { setProgress(false); reload(true); }} />}
    </div>
  );
}

function BackupList({ onRestore, disabled }: { onRestore: (b: any) => void; disabled: boolean }) {
  const { data, error, reload } = useFetch<any>('/backup/list');
  if (error) return <ErrorBox msg={error} />;
  if (!data) return <Spinner />;
  const list: any[] = data.backups || [];
  if (!list.length) return <Empty icon="archive" title="No backups in the bucket yet">Click Back up now to make the first one.</Empty>;
  const del = async (b: any) => {
    if (!(await confirmDialog({ title: 'Delete this backup?', body: `${b.name} is removed from the bucket. This can't be undone.`, confirm: 'Delete', danger: true }))) return;
    if (await attempt(() => api.del('/backup/object?key=' + encodeURIComponent(b.key)), 'Backup deleted')) reload(true);
  };
  return (
    <div class="table-wrap mt">
      <table class="table compact">
        <thead><tr><th>Backup</th><th>Made</th><th>Size</th><th /></tr></thead>
        <tbody>
          {list.map((b) => (
            <tr key={b.key}>
              <td class="mono small">{b.name}</td>
              <td>{fmtDate(b.modified)} <span class="muted small">{timeAgo(b.modified)}</span></td>
              <td>{fmtBytes(b.size)}</td>
              <td class="right nowrap">
                <a class="btn btn-ghost btn-sm" href={'/api/backup/download?key=' + encodeURIComponent(b.key)} title="Download" download><span>Download</span></a>
                <Button size="sm" variant="ghost" icon="history" disabled={disabled} onClick={() => onRestore(b)}>Restore</Button>
                <Button size="sm" variant="ghost" icon="trash" onClick={() => del(b)} title="Delete" />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function RestoreModal({ backup, onClose, onStarted }: { backup: any; onClose: () => void; onStarted: () => void }) {
  const [confirm, setConfirm] = useState('');
  const [pass, setPass] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const go = async () => {
    setBusy(true);
    setErr('');
    try {
      await api.post('/backup/restore', { key: backup.key, passphrase: pass, confirm });
      onStarted();
    } catch (e: any) {
      setErr(e.message);
    }
    setBusy(false);
  };
  return (
    <Modal title="Restore this backup?" onClose={onClose} width={520}
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="danger" loading={busy} disabled={confirm !== 'RESTORE'} onClick={go}>Restore and restart</Button></>}>
      <div class="stack-sm">
        {err && <ErrorBox msg={err} />}
        <p><strong class="mono small">{backup.name}</strong><br /><span class="muted small">made {fmtDate(backup.modified)}, {fmtBytes(backup.size)}</span></p>
        <div class="errorbox small">Everything in Apex is replaced with the contents of this backup: devices, users, settings, scripts, tickets and history.
          Anything changed since it was made is lost, and everyone has to sign in again. Apex restarts when it's done.</div>
        <Field label="Passphrase" hint="Leave empty to use the saved passphrase. Enter the old one if this backup was made before you changed it.">
          <input type="password" autoComplete="off" value={pass} onInput={(e) => setPass((e.target as HTMLInputElement).value)} />
        </Field>
        <Field label='Type RESTORE to confirm'><input value={confirm} autoComplete="off" class="mono" onInput={(e) => setConfirm((e.target as HTMLInputElement).value)} /></Field>
      </div>
    </Modal>
  );
}

/**
 * Follows a restore: shows its progress, waits for the server to restart and
 * then sends the user to sign in.
 */
export function RestoreProgress({ statusPath, onClose, inline }: { statusPath: string; onClose?: () => void; inline?: boolean }) {
  const [phase, setPhase] = useState('Starting');
  const [state, setState] = useState<'running' | 'restarting' | 'done' | 'failed'>('running');
  const [err, setErr] = useState('');
  useEffect(() => {
    let stop = false;
    let boot = '';
    let finished = false; // the restore is done (or the server went away to restart)
    const bootNow = async () => (await (await fetch('/api/setup', { cache: 'no-store' })).json()).boot as string;
    const tick = async () => {
      if (stop) return;
      if (!boot) { try { boot = await bootNow(); } catch { /* retry */ } }
      if (!finished) {
        try {
          // plain fetch: a successful restore signs everyone out (401), which must not redirect this page
          const r = await fetch('/api' + statusPath, { cache: 'no-store', credentials: 'same-origin' });
          if (r.status === 401) throw new Error('signed out by the restore');
          const s = await r.json();
          if (s.state === 'failed') {
            setState('failed');
            setErr(s.error || 'restore failed');
            return;
          }
          if (s.phase) setPhase(s.phase);
          if (s.state === 'done') { finished = true; setState('restarting'); }
        } catch {
          finished = true; // the server went away to restart
          setState('restarting');
        }
      }
      if (finished) {
        try {
          const b = await bootNow();
          if (boot && b && b !== boot) { setState('done'); return; }
        } catch { /* still restarting */ }
      }
      setTimeout(tick, 1000);
    };
    tick();
    return () => { stop = true; };
  }, []);
  const body = (
    <div class="stack-sm restore-progress">
      {state === 'running' && <p><span class="spinner sm" /> {phase}…</p>}
      {state === 'restarting' && <p><span class="spinner sm" /> Restored. Waiting for Apex to restart…</p>}
      {state === 'done' && <p class="text-good">The backup was restored and Apex has restarted. Sign in with an account from the backup.</p>}
      {state === 'failed' && <><ErrorBox msg={err} /><p class="small muted">Nothing was changed.</p></>}
      {state === 'done' && <Button variant="primary" onClick={() => (location.href = '/login')}>Sign in</Button>}
      {state === 'failed' && onClose && <Button onClick={onClose}>Close</Button>}
    </div>
  );
  if (inline) return body;
  return <Modal title="Restoring backup" onClose={() => state === 'failed' && onClose?.()} width={460}>{body}</Modal>;
}
