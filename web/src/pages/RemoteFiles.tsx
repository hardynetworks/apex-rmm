import { useEffect, useRef, useState } from 'preact/hooks';
import { Icon } from '../icons';
import { Button } from '../ui';

// File transfer over the remote-control websocket (see internal/agent/desktop/files.go).

export type Entry = { n: string; d?: boolean; s: number; m: number };
export type Listing = { path: string; parent: string; entries: Entry[]; home: string; roots: string[]; sep: string; truncated?: boolean };
export type Transfer = {
  id: number;
  dir: 'up' | 'down';
  name: string;
  size: number;
  done: number;
  state: 'waiting' | 'active' | 'done' | 'error' | 'cancelled';
  error?: string;
  path?: string;
};

const CHUNK = 256 * 1024;
const WINDOW = 8 * CHUNK;
const TAG_DOWN = 0x10;
const TAG_UP = 0x11;

type Up = { acked: number; wake: (() => void) | null; ready?: { resolve: (path: string) => void; reject: (e: Error) => void }; finished?: { resolve: (path: string) => void; reject: (e: Error) => void }; cancelled?: boolean };

export class FileLink {
  transfers: Transfer[] = [];
  cwd = '';
  private seq = 1;
  private lists = new Map<number, { resolve: (l: Listing) => void; reject: (e: Error) => void }>();
  private downs = new Map<number, { chunks: Uint8Array[]; got: number }>();
  private ups = new Map<number, Up>();
  private upQueue: Promise<void> = Promise.resolve();
  private listeners = new Set<() => void>();
  private emitTimer = 0;
  private closed = false;

  constructor(private ws: WebSocket) {}

  subscribe(fn: () => void) {
    this.listeners.add(fn);
    return () => { this.listeners.delete(fn); };
  }

  private emit(now = true) {
    if (!now) {
      if (!this.emitTimer) this.emitTimer = window.setTimeout(() => this.emit(), 120);
      return;
    }
    clearTimeout(this.emitTimer);
    this.emitTimer = 0;
    this.transfers = this.transfers.slice();
    this.listeners.forEach((f) => f());
  }

  private send(m: any) {
    if (this.ws.readyState === 1) this.ws.send(JSON.stringify(m));
  }

  private t(id: number) {
    return this.transfers.find((t) => t.id === id);
  }

  private update(id: number, patch: Partial<Transfer>, now = true) {
    const t = this.t(id);
    if (t) Object.assign(t, patch);
    this.emit(now);
  }

  list(path: string): Promise<Listing> {
    const id = this.seq++;
    return new Promise((resolve, reject) => {
      if (this.closed) return reject(new Error('Session ended'));
      this.lists.set(id, { resolve, reject });
      this.send({ t: 'fls', id, path });
    });
  }

  download(path: string, name: string) {
    const id = this.seq++;
    this.transfers.unshift({ id, dir: 'down', name, size: 0, done: 0, state: 'active', path });
    this.downs.set(id, { chunks: [], got: 0 });
    this.send({ t: 'fget', id, path });
    this.emit();
  }

  upload(files: File[], dir: string) {
    for (const file of files) {
      const id = this.seq++;
      this.transfers.unshift({ id, dir: 'up', name: file.name, size: file.size, done: 0, state: 'waiting' });
      this.upQueue = this.upQueue.then(() => this.runUpload(id, file, dir)).catch(() => {});
    }
    this.emit();
  }

  private async runUpload(id: number, file: File, dir: string) {
    const t = this.t(id);
    if (!t || t.state !== 'waiting') return;
    if (this.closed) return this.update(id, { state: 'error', error: 'Session ended' });
    const u: Up = { acked: 0, wake: null };
    this.ups.set(id, u);
    this.update(id, { state: 'active' });
    try {
      const path = await new Promise<string>((resolve, reject) => {
        u.ready = { resolve, reject };
        this.send({ t: 'fput', id, path: dir, name: file.name, size: file.size });
      });
      this.update(id, { path, name: path.split(/[\\/]/).pop() || file.name }); // the device may rename it ("name (1).ext")
      const done = new Promise<string>((resolve, reject) => { u.finished = { resolve, reject }; });
      done.catch(() => {});
      let off = 0;
      while (off < file.size) {
        while (!u.cancelled && !this.closed && off - u.acked >= WINDOW) await new Promise<void>((r) => (u.wake = r));
        if (u.cancelled || this.closed) return;
        const data = new Uint8Array(await file.slice(off, off + CHUNK).arrayBuffer());
        const msg = new Uint8Array(5 + data.byteLength);
        msg[0] = TAG_UP;
        new DataView(msg.buffer).setUint32(1, id);
        msg.set(data, 5);
        this.ws.send(msg);
        off += data.byteLength;
      }
      this.send({ t: 'fend', id });
      const final = await done;
      this.update(id, { state: 'done', done: file.size, path: final });
    } catch (e: any) {
      if (!u.cancelled) this.update(id, { state: 'error', error: e.message });
    } finally {
      this.ups.delete(id);
    }
  }

  cancel(id: number) {
    const t = this.t(id);
    if (!t || (t.state !== 'active' && t.state !== 'waiting')) return;
    this.send({ t: 'fcancel', id });
    const u = this.ups.get(id);
    if (u) {
      u.cancelled = true;
      u.wake?.();
      u.ready?.reject(new Error('cancelled'));
      u.finished?.reject(new Error('cancelled'));
    }
    this.downs.delete(id);
    this.update(id, { state: 'cancelled' });
  }

  clearFinished() {
    this.transfers = this.transfers.filter((t) => t.state === 'active' || t.state === 'waiting');
    this.emit();
  }

  /** Handles a JSON message from the agent; returns true if it was a file message. */
  handle(m: any): boolean {
    switch (m.t) {
      case 'fls': {
        const p = this.lists.get(m.id);
        this.lists.delete(m.id);
        p?.resolve({ ...m, entries: m.entries || [], roots: m.roots || [] });
        return true;
      }
      case 'fget':
        this.update(m.id, { size: m.size, name: m.name || this.t(m.id)?.name });
        return true;
      case 'fput':
        this.ups.get(m.id)?.ready?.resolve(m.path);
        return true;
      case 'fack': {
        const u = this.ups.get(m.id);
        if (u) {
          u.acked = m.n;
          u.wake?.();
          u.wake = null;
          this.update(m.id, { done: m.n }, false);
        }
        return true;
      }
      case 'fdone': {
        const d = this.downs.get(m.id);
        if (d) {
          this.downs.delete(m.id);
          const t = this.t(m.id);
          saveBlob(new Blob(d.chunks as BlobPart[]), t?.name || 'download');
          this.update(m.id, { state: 'done', done: d.got, size: d.got });
        }
        this.ups.get(m.id)?.finished?.resolve(m.path);
        return true;
      }
      case 'ferr': {
        const err = new Error(m.error || 'failed');
        const l = this.lists.get(m.id);
        if (l) {
          this.lists.delete(m.id);
          l.reject(err);
          return true;
        }
        const u = this.ups.get(m.id);
        if (u) {
          u.ready?.reject(err);
          u.finished?.reject(err);
        }
        if (this.downs.delete(m.id) || u) this.update(m.id, { state: 'error', error: err.message });
        return true;
      }
    }
    return false;
  }

  /** Handles a binary download chunk (0x10 | u32 id | data); returns true if it was one. */
  chunk(buf: ArrayBuffer): boolean {
    const v = new DataView(buf);
    if (v.getUint8(0) !== TAG_DOWN) return false;
    const id = v.getUint32(1);
    const d = this.downs.get(id);
    if (!d) return true;
    d.chunks.push(new Uint8Array(buf, 5));
    d.got += buf.byteLength - 5;
    this.send({ t: 'fack', id, n: d.got });
    this.update(id, { done: d.got }, false);
    return true;
  }

  close() {
    this.closed = true;
    this.lists.forEach((l) => l.reject(new Error('Session ended')));
    this.lists.clear();
    this.ups.forEach((u) => { u.wake?.(); u.ready?.reject(new Error('Session ended')); u.finished?.reject(new Error('Session ended')); });
    for (const t of this.transfers) if (t.state === 'active' || t.state === 'waiting') { t.state = 'error'; t.error = 'Session ended'; }
    this.downs.clear();
    this.emit();
  }
}

function saveBlob(b: Blob, name: string) {
  const url = URL.createObjectURL(b);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

export function fmtSize(n: number) {
  if (n < 1024) return n + ' B';
  const u = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return (n >= 100 ? n.toFixed(0) : n.toFixed(1)) + ' ' + u[i];
}

function fmtDate(sec: number) {
  if (!sec) return '';
  const d = new Date(sec * 1000);
  const sameYear = d.getFullYear() === new Date().getFullYear();
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', ...(sameYear ? {} : { year: 'numeric' }) }) +
    (sameYear ? ' ' + d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }) : '');
}

function join(dir: string, name: string, sep: string) {
  return dir.endsWith(sep) ? dir + name : dir + sep + name;
}

export function useTransfers(link: FileLink | null) {
  const [, force] = useState(0);
  useEffect(() => link?.subscribe(() => force((n) => n + 1)), [link]);
  return link?.transfers || [];
}

export function FilesPanel({ link, onClose }: { link: FileLink; onClose: () => void }) {
  const [listing, setListing] = useState<Listing | null>(null);
  const [input, setInput] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [drag, setDrag] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const transfers = useTransfers(link);

  const go = async (path: string) => {
    setLoading(true);
    setError('');
    try {
      const l = await link.list(path);
      setListing(l);
      setInput(l.path);
      link.cwd = l.path;
    } catch (e: any) {
      setError(e.message);
      if (listing) setInput(listing.path);
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => { go(link.cwd); }, [link]);

  // Refresh the folder when an upload into it finishes.
  const doneCount = transfers.filter((t) => t.dir === 'up' && t.state === 'done').length;
  useEffect(() => { if (doneCount && listing) go(listing.path); }, [doneCount]);

  const upload = (files: FileList | null) => {
    if (!files || !files.length || !listing) return;
    link.upload(Array.from(files), listing.path);
  };
  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setDrag(false);
    upload(e.dataTransfer?.files || null);
  };

  const sep = listing?.sep || '/';
  const active = transfers.some((t) => t.state === 'active' || t.state === 'waiting');
  return (
    <aside class={'rd-files' + (drag ? ' drag' : '')}
      onDragOver={(e) => { if (e.dataTransfer?.types.includes('Files')) { e.preventDefault(); e.stopPropagation(); setDrag(true); } }}
      onDragLeave={(e) => { if (!(e.currentTarget as HTMLElement).contains(e.relatedTarget as Node)) setDrag(false); }}
      onDrop={onDrop}>
      <div class="rd-files-head">
        <strong><Icon name="folder" size={16} /> Files</strong>
        <div class="rd-files-actions">
          <Button size="sm" variant="primary" icon="upload" onClick={() => fileInput.current?.click()} disabled={!listing}>Upload</Button>
          <button class="rd-icon-btn" onClick={onClose} title="Close"><Icon name="x" size={16} /></button>
        </div>
        <input ref={fileInput} type="file" multiple hidden onChange={(e) => { upload((e.target as HTMLInputElement).files); (e.target as HTMLInputElement).value = ''; }} />
      </div>
      <div class="rd-files-nav">
        <button class="rd-icon-btn" title="Up one folder" disabled={!listing?.parent || loading} onClick={() => listing && go(listing.parent)}><Icon name="up" size={15} /></button>
        <button class="rd-icon-btn" title="Home folder" disabled={loading} onClick={() => go(listing?.home || '')}><Icon name="home" size={15} /></button>
        <button class="rd-icon-btn" title="Refresh" disabled={loading} onClick={() => go(listing?.path || '')}><Icon name="refresh" size={15} /></button>
        {listing && listing.roots.length > 1 && (
          <select value="" onChange={(e) => { const v = (e.target as HTMLSelectElement).value; if (v) go(v); }} title="Drive">
            <option value="">Drive</option>
            {listing.roots.map((r) => <option key={r} value={r}>{r}</option>)}
          </select>
        )}
      </div>
      <form class="rd-files-path" onSubmit={(e) => { e.preventDefault(); go(input.trim()); }}>
        <input value={input} onInput={(e) => setInput((e.target as HTMLInputElement).value)} spellcheck={false} aria-label="Folder path" />
      </form>
      {error && <div class="rd-files-err"><Icon name="alert" size={14} /> {error}</div>}
      <div class="rd-files-list">
        {!listing && loading && <div class="rd-files-empty"><span class="spinner sm" /></div>}
        {listing && !listing.entries.length && <div class="rd-files-empty">This folder is empty</div>}
        {listing?.entries.map((en) => (
          <div key={en.n} class={'rd-file' + (en.d ? ' dir' : '')} title={en.n}
            onClick={() => (en.d ? go(join(listing.path, en.n, sep)) : link.download(join(listing.path, en.n, sep), en.n))}>
            <Icon name={en.d ? 'folder' : 'file'} size={16} />
            <span class="rd-file-name">{en.n}</span>
            <span class="rd-file-meta">{en.d ? '' : fmtSize(en.s)}</span>
            {!en.d && <Icon name="download" size={14} class="rd-file-dl" />}
          </div>
        ))}
        {listing?.truncated && <div class="rd-files-empty">Showing the first 5,000 items</div>}
      </div>
      {transfers.length > 0 && (
        <div class="rd-transfers">
          <div class="rd-transfers-head">
            <span>Transfers</span>
            {!active || transfers.some((t) => t.state !== 'active' && t.state !== 'waiting') ? <button class="rd-link" onClick={() => link.clearFinished()}>Clear finished</button> : null}
          </div>
          {transfers.map((t) => <TransferRow key={t.id} t={t} onCancel={() => link.cancel(t.id)} />)}
        </div>
      )}
      <div class="rd-files-hint">{listing ? <>Drop files here or on the screen to upload to <b>{listing.path}</b>. Click a file to download it.</> : 'Loading…'}</div>
      {drag && <div class="rd-drop"><Icon name="upload" size={28} /><span>Drop to upload to<br /><b>{listing?.path}</b></span></div>}
    </aside>
  );
}

function TransferRow({ t, onCancel }: { t: Transfer; onCancel: () => void }) {
  const pct = t.size ? Math.min(100, Math.round((t.done / t.size) * 100)) : t.state === 'done' ? 100 : 0;
  const live = t.state === 'active' || t.state === 'waiting';
  return (
    <div class={'rd-xfer ' + t.state} title={t.path || t.name}>
      <Icon name={t.dir === 'up' ? 'upload' : 'download'} size={14} />
      <div class="rd-xfer-body">
        <div class="rd-xfer-top">
          <span class="rd-file-name">{t.name}</span>
          <span class="rd-file-meta">
            {t.state === 'error' ? t.error : t.state === 'cancelled' ? 'Cancelled' : t.state === 'waiting' ? 'Queued' : t.state === 'done' ? fmtSize(t.size) : `${pct}% of ${fmtSize(t.size)}`}
          </span>
        </div>
        <div class="rd-bar-track"><div class="rd-bar-fill" style={{ width: pct + '%' }} /></div>
      </div>
      {live ? <button class="rd-icon-btn" onClick={onCancel} title="Cancel"><Icon name="x" size={13} /></button> : t.state === 'done' ? <Icon name="check" size={14} class="ok" /> : null}
    </div>
  );
}
