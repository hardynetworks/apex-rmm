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
import { Settings } from './pages/Settings';
import { UserContext } from './user';

const nav = [
  { href: '/', label: 'Dashboard', icon: 'dashboard' },
  { href: '/devices', label: 'Devices', icon: 'devices' },
  { href: '/alerts', label: 'Alerts', icon: 'bell', badge: 'alerts' },
  { href: '/tickets', label: 'Tickets', icon: 'ticket', badge: 'tickets' },
  { href: '/scripts', label: 'Automation', icon: 'code' },
  { href: '/clients', label: 'Clients', icon: 'building' },
  { href: '/settings', label: 'Settings', icon: 'settings' },
];

function useTheme(): [string, () => void] {
  const [theme, setTheme] = useState<string>(document.documentElement.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));
  const toggle = () => {
    const t = theme === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = t;
    try { localStorage.setItem('hardy-theme', t); } catch {}
    setTheme(t);
  };
  return [theme, toggle];
}

function Shell({ user, children, path }: { user: User; children: ComponentChildren; path: string }) {
  const [theme, toggleTheme] = useTheme();
  const [counts, setCounts] = useState<{ alerts: number; tickets: number }>({ alerts: 0, tickets: 0 });
  const [open, setOpen] = useState(false);
  const [company, setCompany] = useState('Hardy RMM');
  useEffect(() => {
    api.get('/auth/config').then((c) => setCompany(c.company || 'Hardy RMM')).catch(() => {});
    const load = () => api.get('/dashboard').then((d) => setCounts({ alerts: d.alerts?.open || 0, tickets: d.tickets?.open || 0 })).catch(() => {});
    load();
    const t = setInterval(load, 30000);
    return () => clearInterval(t);
  }, []);
  useEffect(() => setOpen(false), [path]);
  const logout = async () => {
    const r = await api.post('/auth/logout');
    location.href = r.redirect || '/login';
  };
  const active = (href: string) => (href === '/' ? path === '/' : path.startsWith(href));
  return (
    <div class={'shell ' + (open ? 'nav-open' : '')}>
      <aside class="sidebar">
        <div class="brand">
          <div class="logo"><svg viewBox="0 0 32 32" width="28" height="28"><rect width="32" height="32" rx="8" fill="var(--accent)" /><path d="M9 8v16M23 8v16M9 16h14" stroke="#fff" stroke-width="3.2" stroke-linecap="round" /></svg></div>
          <div>
            <strong>{company}</strong>
            <small>Remote Management</small>
          </div>
        </div>
        <nav>
          {nav.map((n) => (
            <Link key={n.href} href={n.href} class={'nav-item ' + (active(n.href) ? 'active' : '')}>
              <Icon name={n.icon} size={19} />
              <span>{n.label}</span>
              {n.badge && (counts as any)[n.badge] > 0 && <span class="nav-badge">{(counts as any)[n.badge]}</span>}
            </Link>
          ))}
        </nav>
        <div class="sidebar-foot">
          <div class="me">
            <div class="avatar">{(user.name || user.email || '?').slice(0, 1).toUpperCase()}</div>
            <div class="me-text">
              <strong>{user.name || user.email}</strong>
              <small>{user.role}{user.local ? ' · local' : ' · SSO'}</small>
            </div>
          </div>
          <div class="foot-actions">
            <button class="icon-btn" title={theme === 'dark' ? 'Light mode' : 'Dark mode'} onClick={toggleTheme}><Icon name={theme === 'dark' ? 'sun' : 'moon'} /></button>
            <button class="icon-btn" title="Sign out" onClick={logout}><Icon name="logout" /></button>
          </div>
        </div>
      </aside>
      <div class="scrim" onClick={() => setOpen(false)} />
      <main class="main">
        <div class="mobile-bar">
          <button class="icon-btn" onClick={() => setOpen(true)} aria-label="Menu"><Icon name="menu" /></button>
          <strong>{company}</strong>
        </div>
        {children}
      </main>
    </div>
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
  if ((m = match('/devices/:id', path))) return <DevicePage id={m.id} />;
  if ((m = match('/devices/:id/terminal', path))) return <TerminalPage id={m.id} />;
  if (path === '/scripts') return <Scripts />;
  if ((m = match('/jobs/:id', path))) return <JobPage id={m.id} />;
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
