import type { ComponentChildren, JSX } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import { api } from './api';
import { Icon } from './icons';

// ---------- formatting ----------

export function fmtBytes(n: number | null | undefined, dp = 1): string {
  if (!n || n <= 0) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), u.length - 1);
  return (n / Math.pow(1024, i)).toFixed(i === 0 ? 0 : dp) + ' ' + u[i];
}

export function timeAgo(t: string | null | undefined): string {
  if (!t) return 'never';
  const s = (Date.now() - new Date(t).getTime()) / 1000;
  if (s < 0) return 'just now';
  if (s < 45) return 'just now';
  if (s < 3600) return Math.round(s / 60) + 'm ago';
  if (s < 86400) return Math.round(s / 3600) + 'h ago';
  if (s < 86400 * 30) return Math.round(s / 86400) + 'd ago';
  return new Date(t).toLocaleDateString();
}

export function fmtDate(t: string | null | undefined): string {
  if (!t) return '—';
  return new Date(t).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

export function fmtUptime(bootTime: string | null | undefined): string {
  if (!bootTime) return '—';
  let s = (Date.now() - new Date(bootTime).getTime()) / 1000;
  const d = Math.floor(s / 86400);
  s -= d * 86400;
  const h = Math.floor(s / 3600);
  const m = Math.floor((s - h * 3600) / 60);
  return d > 0 ? `${d}d ${h}h` : h > 0 ? `${h}h ${m}m` : `${m}m`;
}

export const osLabel = (os: string) => ({ windows: 'Windows', darwin: 'macOS', linux: 'Linux' } as Record<string, string>)[os] || os || 'Unknown';

export function deviceName(d: any): string {
  return d?.display_name || d?.hostname || 'Unnamed device';
}

// ---------- data hooks ----------

export function useFetch<T = any>(path: string | null, deps: any[] = [], intervalMs = 0) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const seq = useRef(0);
  const load = useCallback(async (quiet = false) => {
    if (!path) return;
    const my = ++seq.current;
    if (!quiet) setLoading(true);
    try {
      const d = await api.get<T>(path);
      if (my === seq.current) {
        setData(d);
        setError(null);
      }
    } catch (e: any) {
      if (my === seq.current) setError(e.message);
    } finally {
      if (my === seq.current) setLoading(false);
    }
  }, [path, ...deps]);
  useEffect(() => {
    load();
    if (intervalMs > 0) {
      const t = setInterval(() => document.visibilityState === 'visible' && load(true), intervalMs);
      return () => clearInterval(t);
    }
  }, [load, intervalMs]);
  return { data, error, loading, reload: load, setData };
}

// ---------- toasts ----------

type Toast = { id: number; msg: string; kind: 'ok' | 'error' | 'info' };
let toastSetter: ((f: (t: Toast[]) => Toast[]) => void) | null = null;
let toastId = 0;

export function toast(msg: string, kind: Toast['kind'] = 'ok') {
  const id = ++toastId;
  toastSetter?.((t) => [...t, { id, msg, kind }]);
  setTimeout(() => toastSetter?.((t) => t.filter((x) => x.id !== id)), kind === 'error' ? 7000 : 3500);
}

export function Toasts() {
  const [items, setItems] = useState<Toast[]>([]);
  toastSetter = setItems;
  return (
    <div class="toasts" role="status" aria-live="polite">
      {items.map((t) => (
        <div key={t.id} class={'toast toast-' + t.kind}>
          <Icon name={t.kind === 'error' ? 'alert' : t.kind === 'info' ? 'info' : 'check'} size={16} />
          <span>{t.msg}</span>
        </div>
      ))}
    </div>
  );
}

/** Runs an async action, toasting errors; returns true on success. */
export async function attempt(fn: () => Promise<any>, okMsg?: string): Promise<boolean> {
  try {
    await fn();
    if (okMsg) toast(okMsg);
    return true;
  } catch (e: any) {
    toast(e.message || String(e), 'error');
    return false;
  }
}

// ---------- confirm dialog ----------

type ConfirmReq = { title: string; body?: string; confirm?: string; danger?: boolean; resolve: (v: boolean) => void };
let confirmSetter: ((r: ConfirmReq | null) => void) | null = null;

export function confirmDialog(opts: { title: string; body?: string; confirm?: string; danger?: boolean }): Promise<boolean> {
  return new Promise((resolve) => confirmSetter?.({ ...opts, resolve }));
}

export function ConfirmHost() {
  const [req, setReq] = useState<ConfirmReq | null>(null);
  confirmSetter = setReq;
  if (!req) return null;
  const done = (v: boolean) => {
    req.resolve(v);
    setReq(null);
  };
  return (
    <Modal title={req.title} onClose={() => done(false)} width={420}
      footer={<>
        <Button onClick={() => done(false)}>Cancel</Button>
        <Button variant={req.danger ? 'danger' : 'primary'} onClick={() => done(true)} autoFocus>{req.confirm || 'Confirm'}</Button>
      </>}>
      {req.body && <p class="muted">{req.body}</p>}
    </Modal>
  );
}

// ---------- primitives ----------

type BtnProps = JSX.ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: 'primary' | 'danger' | 'ghost' | 'default' | 'subtle';
  icon?: string;
  size?: 'sm' | 'md';
  loading?: boolean;
};

export function Button({ variant = 'default', icon, size = 'md', loading, children, class: cls, disabled, ...rest }: BtnProps) {
  return (
    <button type="button" {...rest} disabled={disabled || loading} class={`btn btn-${variant} btn-${size} ${cls || ''}`}>
      {loading ? <span class="spinner sm" /> : icon ? <Icon name={icon} size={size === 'sm' ? 15 : 17} /> : null}
      {children && <span>{children}</span>}
    </button>
  );
}

export function Card({ title, actions, children, class: cls, pad = true }: { title?: ComponentChildren; actions?: ComponentChildren; children?: ComponentChildren; class?: string; pad?: boolean }) {
  return (
    <section class={'card ' + (cls || '')}>
      {(title || actions) && (
        <header class="card-head">
          <h3>{title}</h3>
          <div class="card-actions">{actions}</div>
        </header>
      )}
      <div class={pad ? 'card-body' : ''}>{children}</div>
    </section>
  );
}

export function Badge({ tone = 'neutral', children }: { tone?: string; children: ComponentChildren }) {
  return <span class={'badge badge-' + tone}>{children}</span>;
}

export function StatusDot({ online, label }: { online: boolean; label?: boolean }) {
  return (
    <span class={'status ' + (online ? 'status-on' : 'status-off')}>
      <span class="dot" />
      {label !== false && (online ? 'Online' : 'Offline')}
    </span>
  );
}

export function OsBadge({ os }: { os: string }) {
  const icon = os === 'linux' ? 'server' : os === 'darwin' ? 'laptop' : 'devices';
  return (
    <span class={'os os-' + os}>
      <Icon name={icon} size={14} />
      {osLabel(os)}
    </span>
  );
}

const sevTone: Record<string, string> = { critical: 'critical', warning: 'warning', info: 'info' };
export function Severity({ s }: { s: string }) {
  return <Badge tone={sevTone[s] || 'neutral'}>{s}</Badge>;
}

const prioTone: Record<string, string> = { urgent: 'critical', high: 'serious', medium: 'info', low: 'neutral' };
export function Priority({ p }: { p: string }) {
  return <Badge tone={prioTone[p] || 'neutral'}>{p}</Badge>;
}

const statusTone: Record<string, string> = {
  open: 'info', in_progress: 'accent', waiting: 'warning', resolved: 'good', closed: 'neutral',
  success: 'good', failed: 'critical', timeout: 'serious', error: 'critical', pending: 'neutral', running: 'accent', expired: 'neutral',
  acknowledged: 'warning',
};
export function StatusBadge({ s }: { s: string }) {
  return <Badge tone={statusTone[s] || 'neutral'}>{s.replace('_', ' ')}</Badge>;
}

export function Meter({ value, warn = 80, crit = 90 }: { value: number; warn?: number; crit?: number }) {
  const v = Math.max(0, Math.min(100, value || 0));
  const tone = v >= crit ? 'crit' : v >= warn ? 'warn' : 'ok';
  return (
    <div class="meter" title={v.toFixed(1) + '%'}>
      <div class={'meter-bar meter-' + tone} style={{ width: v + '%' }} />
      <span class="meter-label">{Math.round(v)}%</span>
    </div>
  );
}

export function Spinner({ label }: { label?: string }) {
  return (
    <div class="loading">
      <span class="spinner" />
      {label && <span>{label}</span>}
    </div>
  );
}

export function Empty({ icon = 'box', title, children }: { icon?: string; title: string; children?: ComponentChildren }) {
  return (
    <div class="empty">
      <Icon name={icon} size={30} />
      <strong>{title}</strong>
      {children && <div class="muted">{children}</div>}
    </div>
  );
}

export function ErrorBox({ msg }: { msg: string }) {
  return (
    <div class="errorbox">
      <Icon name="alert" size={16} /> {msg}
    </div>
  );
}

export function Modal({ title, onClose, children, footer, width = 560 }: { title: ComponentChildren; onClose: () => void; children?: ComponentChildren; footer?: ComponentChildren; width?: number }) {
  useEffect(() => {
    const k = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', k);
    return () => window.removeEventListener('keydown', k);
  }, [onClose]);
  return (
    <div class="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div class="modal" style={{ maxWidth: width + 'px' }} role="dialog" aria-modal="true">
        <header class="modal-head">
          <h2>{title}</h2>
          <button class="icon-btn" onClick={onClose} aria-label="Close">
            <Icon name="x" />
          </button>
        </header>
        <div class="modal-body">{children}</div>
        {footer && <footer class="modal-foot">{footer}</footer>}
      </div>
    </div>
  );
}

export function Field({ label, hint, children }: { label: string; hint?: ComponentChildren; children: ComponentChildren }) {
  return (
    <label class="field">
      <span class="field-label">{label}</span>
      {children}
      {hint && <span class="field-hint">{hint}</span>}
    </label>
  );
}

export function Tabs({ tabs, active, onChange }: { tabs: { id: string; label: string; icon?: string; count?: number }[]; active: string; onChange: (id: string) => void }) {
  return (
    <div class="tabs" role="tablist">
      {tabs.map((t) => (
        <button key={t.id} role="tab" aria-selected={active === t.id} class={'tab ' + (active === t.id ? 'active' : '')} onClick={() => onChange(t.id)}>
          {t.icon && <Icon name={t.icon} size={15} />}
          {t.label}
          {t.count ? <span class="tab-count">{t.count}</span> : null}
        </button>
      ))}
    </div>
  );
}

export function PageHeader({ title, subtitle, actions, back }: { title: ComponentChildren; subtitle?: ComponentChildren; actions?: ComponentChildren; back?: ComponentChildren }) {
  return (
    <div class="page-head">
      <div>
        {back}
        <h1>{title}</h1>
        {subtitle && <p class="muted">{subtitle}</p>}
      </div>
      <div class="page-actions">{actions}</div>
    </div>
  );
}

export function SearchInput({ value, onInput, placeholder = 'Search…' }: { value: string; onInput: (v: string) => void; placeholder?: string }) {
  return (
    <div class="search">
      <Icon name="search" size={16} />
      <input type="search" value={value} placeholder={placeholder} onInput={(e) => onInput((e.target as HTMLInputElement).value)} />
    </div>
  );
}

export function CopyText({ text, mono = true }: { text: string; mono?: boolean }) {
  return (
    <div class={'copytext ' + (mono ? 'mono' : '')}>
      <span>{text}</span>
      <button class="icon-btn" title="Copy" onClick={() => navigator.clipboard.writeText(text).then(() => toast('Copied to clipboard'))}>
        <Icon name="copy" size={15} />
      </button>
    </div>
  );
}

export function Kv({ items }: { items: [string, ComponentChildren][] }) {
  return (
    <dl class="kv">
      {items.map(([k, v]) => (
        <div key={k}>
          <dt>{k}</dt>
          <dd>{v ?? '—'}</dd>
        </div>
      ))}
    </dl>
  );
}
