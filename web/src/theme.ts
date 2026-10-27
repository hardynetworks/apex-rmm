import { useEffect, useState } from 'preact/hooks';

export type ThemePref = 'dark' | 'light' | 'system';

const KEY = 'apex-theme';
const subs = new Set<(p: ThemePref) => void>();

/** The saved choice; missing or unknown values fall back to dark (the default look). */
export function getThemePref(): ThemePref {
  try {
    const v = localStorage.getItem(KEY);
    if (v === 'dark' || v === 'light' || v === 'system') return v;
  } catch {}
  return 'dark';
}

function apply(p: ThemePref) {
  // no attribute = styles.css follows the OS setting
  if (p === 'system') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = p;
}

export function setThemePref(p: ThemePref) {
  try { localStorage.setItem(KEY, p); } catch {}
  apply(p);
  subs.forEach((fn) => fn(p));
}

function osDark(): boolean {
  try { return window.matchMedia('(prefers-color-scheme: dark)').matches; } catch { return true; }
}

/** What is actually on screen for a given choice. */
export function effectiveTheme(pref: ThemePref): 'dark' | 'light' {
  if (pref === 'system') return osDark() ? 'dark' : 'light';
  return pref;
}

export function useTheme(): { pref: ThemePref; effective: 'dark' | 'light'; setPref: (p: ThemePref) => void } {
  const [pref, setPrefState] = useState<ThemePref>(getThemePref);
  const [, setTick] = useState(0);
  useEffect(() => {
    subs.add(setPrefState);
    return () => { subs.delete(setPrefState); };
  }, []);
  useEffect(() => {
    if (pref !== 'system') return;
    let mq: MediaQueryList;
    try { mq = window.matchMedia('(prefers-color-scheme: dark)'); } catch { return; }
    const onChange = () => setTick((n) => n + 1);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, [pref]);
  return { pref, effective: effectiveTheme(pref), setPref: setThemePref };
}
