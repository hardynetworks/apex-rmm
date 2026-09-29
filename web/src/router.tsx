import { useEffect, useState } from 'preact/hooks';
import type { ComponentChildren } from 'preact';

// Minimal History-API router.

const listeners = new Set<() => void>();

export function navigate(to: string, replace = false) {
  if (replace) history.replaceState(null, '', to);
  else history.pushState(null, '', to);
  listeners.forEach((l) => l());
  window.scrollTo(0, 0);
}

window.addEventListener('popstate', () => listeners.forEach((l) => l()));

export function useLocation() {
  const [loc, setLoc] = useState({ path: location.pathname, search: location.search });
  useEffect(() => {
    const l = () => setLoc({ path: location.pathname, search: location.search });
    listeners.add(l);
    return () => void listeners.delete(l);
  }, []);
  return loc;
}

export function useQuery(): URLSearchParams {
  const { search } = useLocation();
  return new URLSearchParams(search);
}

/** match("/devices/:id", "/devices/abc") -> {id:"abc"} */
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split('/').filter(Boolean);
  const s = path.split('/').filter(Boolean);
  if (p.length !== s.length) return null;
  const params: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    if (p[i].startsWith(':')) params[p[i].slice(1)] = decodeURIComponent(s[i]);
    else if (p[i] !== s[i]) return null;
  }
  return params;
}

export function Link(props: { href: string; class?: string; children?: ComponentChildren; title?: string; onClick?: () => void }) {
  return (
    <a
      href={props.href}
      class={props.class}
      title={props.title}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || (e as MouseEvent).button !== 0) return;
        e.preventDefault();
        props.onClick?.();
        navigate(props.href);
      }}
    >
      {props.children}
    </a>
  );
}
