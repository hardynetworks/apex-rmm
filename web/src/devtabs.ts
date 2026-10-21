import { useEffect, useState } from 'preact/hooks';

// Device tabs in the top bar: every device page you open gets a tab (like a browser), so you can
// jump between devices. Each tab remembers the device's sub-tab (Terminal, Processes…).
// Kept per browser in localStorage.

export type DeviceTab = { id: string; name: string; os?: string; sub?: string };

const KEY = 'apex-device-tabs';
const MAX = 15;
const listeners = new Set<() => void>();

function load(): DeviceTab[] {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) || '[]');
    return Array.isArray(v) ? v.filter((t) => t && typeof t.id === 'string').slice(0, MAX) : [];
  } catch {
    return [];
  }
}

let tabs: DeviceTab[] = load();

function save(next: DeviceTab[]) {
  tabs = next;
  try {
    localStorage.setItem(KEY, JSON.stringify(tabs));
  } catch {
    /* private mode: tabs last for this page load */
  }
  listeners.forEach((l) => l());
}

// Other browser windows (e.g. a second dashboard window) see the same tabs.
window.addEventListener('storage', (e) => {
  if (e.key === KEY) {
    tabs = load();
    listeners.forEach((l) => l());
  }
});

/** Adds the device's tab (or refreshes its name and sub-tab). New tabs go after the active one. */
export function openDeviceTab(t: DeviceTab, afterId?: string) {
  const i = tabs.findIndex((x) => x.id === t.id);
  if (i >= 0) {
    const cur = tabs[i];
    if (cur.name === t.name && cur.os === t.os && (t.sub == null || cur.sub === t.sub)) return;
    const next = [...tabs];
    next[i] = { ...cur, ...t, sub: t.sub ?? cur.sub };
    save(next);
    return;
  }
  const next = [...tabs];
  const at = afterId ? next.findIndex((x) => x.id === afterId) : -1;
  next.splice(at >= 0 ? at + 1 : next.length, 0, t);
  // over the limit: drop the oldest tabs that aren't the new one
  while (next.length > MAX) next.splice(next[0].id === t.id ? 1 : 0, 1);
  save(next);
}

// The device last shown in this browser tab (survives reloads, not shared between windows).
const LAST = 'apex-device-tab-last';

/** Called by the device page: opens its tab next to the device you came from. */
export function visitDevice(t: DeviceTab) {
  let last = '';
  try { last = sessionStorage.getItem(LAST) || ''; } catch { /* ignore */ }
  openDeviceTab(t, last || undefined);
  try { sessionStorage.setItem(LAST, t.id); } catch { /* ignore */ }
}

export function setDeviceTabSub(id: string, sub: string) {
  const i = tabs.findIndex((x) => x.id === id);
  if (i < 0 || tabs[i].sub === sub) return;
  const next = [...tabs];
  next[i] = { ...next[i], sub };
  save(next);
}

/** Closes a tab; returns the tab to show instead (its right neighbour, else the left one). */
export function closeDeviceTab(id: string): DeviceTab | null {
  const i = tabs.findIndex((x) => x.id === id);
  if (i < 0) return null;
  const next = tabs.filter((x) => x.id !== id);
  save(next);
  return next[i] || next[i - 1] || null;
}

export function closeAllDeviceTabs() {
  save([]);
}

export function moveDeviceTab(id: string, toIndex: number) {
  const i = tabs.findIndex((x) => x.id === id);
  if (i < 0) return;
  const next = [...tabs];
  const [t] = next.splice(i, 1);
  next.splice(Math.max(0, Math.min(toIndex, next.length)), 0, t);
  save(next);
}

export function deviceTabHref(t: DeviceTab) {
  return `/devices/${t.id}${t.sub && t.sub !== 'overview' ? '?tab=' + t.sub : ''}`;
}

export function useDeviceTabs(): DeviceTab[] {
  const [, force] = useState(0);
  useEffect(() => {
    const l = () => force((n) => n + 1);
    listeners.add(l);
    return () => void listeners.delete(l);
  }, []);
  return tabs;
}
