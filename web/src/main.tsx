import { Component, render } from 'preact';
import type { ComponentChildren } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import './styles.css';
import { api, User } from './api';
import { Icon } from './icons';
import { Link, match, navigate, useLocation } from './router';
import { ConfirmHost, Spinner, Toasts } from './ui';
import { Login } from './pages/Login';
import { Setup } from './pages/Setup';
import { Dashboard } from './pages/Dashboard';
import { Devices } from './pages/Devices';
import { DevicePage } from './pages/Device';
import { RemoteDesktop } from './pages/RemoteDesktop';
import { TerminalPage } from './pages/Terminal';
import { Scripts, JobPage } from './pages/Scripts';
import { Alerts } from './pages/Alerts';
import { Tickets, TicketPage } from './pages/Tickets';
import { Clients } from './pages/Clients';
import { Virtualization } from './pages/Virtualization';
import { Settings } from './pages/Settings';
import { UserContext } from './user';
import { native } from './native';
import { closeAllDeviceTabs, closeDeviceTab, deviceTabHref, moveDeviceTab, useDeviceTabs } from './devtabs';

const nav = [
  { href: '/', label: 'Dashboard', icon: 'dashboard' },
  { href: '/devices', label: 'Devices', icon: 'devices' },
  { href: '/virtualization', label: 'Virtualization', icon: 'server' },
  { href: '/alerts', label: 'Alerts', icon: 'bell', badge: 'alerts' },
  { href: '/tickets', label: 'Tickets', icon: 'ticket', badge: 'tickets' },
  { href: '/scripts', label: 'Automation', icon: 'code' },
  { href: '/clients', label: 'Clients', icon: 'building' },
  { href: '/settings', label: 'Settings', icon: 'settings' },
];

function useTheme(): [string, () => void] {
  // dark is the default look; light stays available from the top bar
  const [theme, setTheme] = useState<string>(document.documentElement.dataset.theme || 'dark');
  const toggle = () => {
    const t = theme === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = t;
    try { localStorage.setItem('apex-theme', t); } catch {}
    setTheme(t);
  };
  return [theme, toggle];
}

// Desktop app: show a Windows notification when new alerts come in.
function useAlertNotifications() {
  useEffect(() => {
    if (!native || native.kind !== 'main') return;
    let seen: Set<string> | null = null;
    const poll = () =>
      api.get<any[]>('/alerts?status=open').then((rows) => {
        if (!Array.isArray(rows)) return;
        if (seen === null) { seen = new Set(rows.map((r) => r.id)); return; } // don't announce what was already open
        const fresh = rows.filter((r) => !seen!.has(r.id));
        rows.forEach((r) => seen!.add(r.id));
        if (fresh.length === 1) {
          const r = fresh[0];
          const sev = r.severity === 'critical' ? 'Critical' : r.severity === 'warning' ? 'Warning' : 'Alert';
          native!.notify(`${sev}: ${r.title}`, [r.device_name, r.message].filter(Boolean).join(' — '), '/alerts').catch(() => {});
        } else if (fresh.length > 1) {
          native!.notify(`${fresh.length} new alerts`, fresh.slice(0, 3).map((r) => r.title).join('\n'), '/alerts').catch(() => {});
        }
      }).catch(() => {});
    poll();
    const t = setInterval(poll, 30000);
    return () => clearInterval(t);
  }, []);
}

function Shell({ user, children, path }: { user: User; children: ComponentChildren; path: string }) {
  const [theme, toggleTheme] = useTheme();
  const [counts, setCounts] = useState<{ alerts: number; tickets: number }>({ alerts: 0, tickets: 0 });
  const [open, setOpen] = useState(false);
  const [company, setCompany] = useState('Apex RMM');
  useEffect(() => {
    api.get('/auth/config').then((c) => setCompany(c.company || 'Apex RMM')).catch(() => {});
    const load = () => api.get('/dashboard').then((d) => setCounts({ alerts: d.alerts?.open || 0, tickets: d.tickets?.open || 0 })).catch(() => {});
    load();
    const t = setInterval(load, 30000);
    return () => clearInterval(t);
  }, []);
  useEffect(() => setOpen(false), [path]);
  useAlertNotifications();
  const logout = async () => {
    const r = await api.post('/auth/logout');
    location.href = r.redirect || '/login';
  };
  const active = (href: string) => (href === '/' ? path === '/' : path.startsWith(href));
  const current = nav.find((n) => active(n.href));
  const initials = (user.name || user.email || '?').split(/[\s@.]+/).filter(Boolean).slice(0, 2).map((w) => w[0]).join('').toUpperCase();
  return (
    <div class={'shell ' + (open ? 'nav-open' : '')}>
      <aside class="sidebar">
        <div class="brand">
          <div class="logo glow">
            <svg viewBox="0 0 32 32" width="22" height="22" aria-hidden="true"><path d="M8.5 24L16 8l7.5 16M11.6 18.5h8.8" fill="none" stroke="currentColor" stroke-width="3.2" stroke-linecap="round" stroke-linejoin="round" /></svg>
          </div>
          <div class="brand-text">
            <strong>{company}</strong>
            <small>Remote Management</small>
          </div>
        </div>
        <nav>
          {nav.map((n) => {
            const on = active(n.href);
            const count = n.badge ? (counts as any)[n.badge] : 0;
            return (
              <Link key={n.href} href={n.href} class={'nav-item ' + (on ? 'active' : '')}>
                <Icon name={n.icon} size={19} />
                <span>{n.label}</span>
                {count > 0 ? <span class="nav-badge">{count}</span> : on && <span class="nav-dot" />}
              </Link>
            );
          })}
        </nav>
        <div class="sidebar-foot">
          <div class="me">
            <div class="avatar">{initials}</div>
            <div class="me-text">
              <strong>{user.name || user.email}</strong>
              <small>{user.email && user.name ? user.email : user.role + (user.local ? ' · local' : ' · SSO')}</small>
            </div>
          </div>
        </div>
      </aside>
      <div class="scrim" onClick={() => setOpen(false)} />
      <main class="main">
        <TopBar title={current?.label || company} user={user} initials={initials} alerts={counts.alerts} theme={theme}
          onTheme={toggleTheme} onMenu={() => setOpen(true)} onLogout={logout} />
        <div class="main-scroll">{children}</div>
      </main>
    </div>
  );
}

function TopBar({ title, user, initials, alerts, theme, onTheme, onMenu, onLogout }: {
  title: string; user: User; initials: string; alerts: number; theme: string;
  onTheme: () => void; onMenu: () => void; onLogout: () => void;
}) {
  const [menu, setMenu] = useState(false);
  useEffect(() => {
    if (!menu) return;
    const close = () => setMenu(false);
    const t = setTimeout(() => document.addEventListener('click', close), 0);
    return () => { clearTimeout(t); document.removeEventListener('click', close); };
  }, [menu]);
  return (
    <header class="topbar">
      <button class="icon-btn topbar-menu" onClick={onMenu} aria-label="Menu"><Icon name="menu" /></button>
      <DeviceTabs title={title} />
      <div class="topbar-actions">
        <Link href="/alerts" class="icon-btn topbar-bell" title={alerts ? `${alerts} open alerts` : 'Alerts'} aria-label="Alerts">
          <Icon name="bell" size={19} />
          {alerts > 0 && <span class="pulse-dot" />}
        </Link>
        <button class="icon-btn" title={theme === 'dark' ? 'Light mode' : 'Dark mode'} onClick={onTheme}><Icon name={theme === 'dark' ? 'sun' : 'moon'} /></button>
        <div class="menu-wrap">
          <button class="profile-btn" onClick={() => setMenu(!menu)} aria-haspopup="menu" aria-expanded={menu}>
            <span class="avatar sm-grad">{initials}</span>
            <Icon name="chevronDown" size={15} />
          </button>
          {menu && (
            <div class="menu" role="menu">
              <div class="menu-head"><strong>{user.name || user.email}</strong><span class="muted small">{user.role}{user.local ? ' · local account' : ' · SSO'}</span></div>
              <hr />
              <Link href="/settings?tab=users" class="menu-link"><Icon name="user" size={16} /> Account</Link>
              <button class="danger" onClick={onLogout}><Icon name="logout" size={16} /> Sign out</button>
            </div>
          )}
        </div>
      </div>
    </header>
  );
}

/** Browser-style tabs for the devices you have open; the page title when there are none. */
function DeviceTabs({ title }: { title: string }) {
  const tabs = useDeviceTabs();
  const { path } = useLocation();
  const activeId = match('/devices/:id', path)?.id || match('/devices/:id/terminal', path)?.id || '';
  const [online, setOnline] = useState<Record<string, boolean | null>>({});
  const [drag, setDrag] = useState<string | null>(null);
  const ids = tabs.map((t) => t.id).join(',');
  useEffect(() => {
    if (!ids) return;
    const load = () => api.get<any[]>('/devices?ids=' + encodeURIComponent(ids)).then((rows) => {
      const m: Record<string, boolean | null> = {};
      for (const id of ids.split(',')) m[id] = null; // null: removed
      for (const r of rows) m[r.id] = !!r.online;
      setOnline(m);
    }).catch(() => {});
    load();
    const t = setInterval(() => document.visibilityState === 'visible' && load(), 30000);
    return () => clearInterval(t);
  }, [ids]);
  // keep the active tab in view
  useEffect(() => {
    document.querySelector('.dev-tab.active')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }, [activeId, tabs.length]);
  if (!tabs.length) return <h2 class="topbar-title">{title}</h2>;
  const close = (id: string) => {
    const next = closeDeviceTab(id);
    if (id === activeId) navigate(next ? deviceTabHref(next) : '/devices');
  };
  return (
    <nav class="dev-tabs" aria-label="Open devices">
      {tabs.map((t, i) => {
        const on = online[t.id];
        return (
          <div key={t.id} class={'dev-tab' + (t.id === activeId ? ' active' : '') + (on === null ? ' gone' : '') + (drag === t.id ? ' dragging' : '')}
            draggable onDragStart={(e) => { setDrag(t.id); e.dataTransfer?.setData('text/plain', t.id); }} onDragEnd={() => setDrag(null)}
            onDragOver={(e) => drag && e.preventDefault()} onDrop={(e) => { e.preventDefault(); if (drag) moveDeviceTab(drag, i); setDrag(null); }}
            onAuxClick={(e) => { if (e.button === 1) { e.preventDefault(); close(t.id); } }}>
            <Link href={deviceTabHref(t)} title={t.name + (on === false ? ' (offline)' : on === null ? ' (removed)' : '')}>
              <span class={'dev-tab-dot ' + (on ? 'on' : on === false ? 'off' : 'unknown')} />
              <span class="dev-tab-name">{t.name}</span>
            </Link>
            <button class="dev-tab-x" aria-label={'Close ' + t.name} title="Close tab" onClick={() => close(t.id)}><Icon name="x" size={13} /></button>
          </div>
        );
      })}
      {tabs.length > 1 && <button class="dev-tabs-clear" title="Close all device tabs" onClick={() => { closeAllDeviceTabs(); if (activeId) navigate('/devices'); }}>Close all</button>}
    </nav>
  );
}

class Boundary extends Component<{ path: string; children?: ComponentChildren }, { err: string | null }> {
  state = { err: null as string | null };
  componentDidCatch(e: any) { this.setState({ err: String(e?.message || e) }); }
  componentDidUpdate(prev: any) { if (prev.path !== this.props.path && this.state.err) this.setState({ err: null }); }
  render() {
    if (this.state.err) return <div class="page"><div class="errorbox">Something went wrong rendering this page: {this.state.err}</div></div>;
    return this.props.children;
  }
}

function Routes({ path }: { path: string }) {
  let m: Record<string, string> | null;
  if (path === '/') return <Dashboard />;
  if (path === '/devices') return <Devices />;
  if ((m = match('/devices/:id', path))) return <DevicePage key={m.id} id={m.id} />;
  if ((m = match('/devices/:id/terminal', path))) return <TerminalPage id={m.id} />;
  if (path === '/scripts') return <Scripts />;
  if ((m = match('/jobs/:id', path))) return <JobPage id={m.id} />;
  if (path === '/virtualization') return <Virtualization />;
  if (path === '/alerts') return <Alerts />;
  if (path === '/tickets') return <Tickets />;
  if ((m = match('/tickets/:id', path))) return <TicketPage id={m.id} />;
  if (path === '/clients') return <Clients />;
  if (path === '/settings') return <Settings />;
  return (
    <div class="page">
      <h1>Not found</h1>
      <p><Link href="/">Back to dashboard</Link></p>
    </div>
  );
}

function App() {
  const { path } = useLocation();
  const [user, setUser] = useState<User | null | undefined>(undefined);
  useEffect(() => {
    if (path === '/login' || path === '/setup') return;
    api.get<User>('/auth/me').then(setUser).catch(() => setUser(null));
  }, [path === '/login' || path === '/setup']);

  if (path === '/login') return <Login />;
  if (path === '/setup') return <Setup />;
  if (user === undefined) return <div class="boot"><Spinner label="Loading…" /></div>;
  if (user === null) {
    navigate('/login?return=' + encodeURIComponent(path), true);
    return null;
  }
  // Remote desktop is a full-screen view without the shell.
  const rd = match('/devices/:id/remote', path);
  return (
    <UserContext.Provider value={user}>
      {rd ? <RemoteDesktop id={rd.id} /> : (
        <Shell user={user} path={path}>
          <Boundary path={path}><Routes path={path} /></Boundary>
        </Shell>
      )}
      <Toasts />
      <ConfirmHost />
    </UserContext.Provider>
  );
}

render(<App />, document.getElementById('app')!);
