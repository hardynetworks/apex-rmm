import { useEffect, useState } from 'preact/hooks';
import { api } from '../api';
import { navigate } from '../router';
import { Icon } from '../icons';
import { Button, ErrorBox } from '../ui';

export function Login() {
  const q = new URLSearchParams(location.search);
  const ret = q.get('return') || '/';
  const [cfg, setCfg] = useState<{ sso: boolean; local: boolean; company: string } | null>(null);
  const [showLocal, setShowLocal] = useState(false);
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [err, setErr] = useState(q.get('error') === 'sso_disabled' ? 'Single sign-on is not configured on this server.' : '');
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.get('/auth/config').then((c) => {
      if (c.setup) return navigate('/setup', true);
      setCfg(c);
      if (!c.sso) setShowLocal(true);
    });
  }, []);

  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setErr('');
    try {
      await api.post('/auth/local', { email, password });
      location.href = ret;
    } catch (e: any) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="login">
      <div class="login-card">
        <div class="login-brand">
          <svg viewBox="0 0 32 32" width="44" height="44"><rect width="32" height="32" rx="8" fill="var(--accent)" /><path d="M9 8v16M23 8v16M9 16h14" stroke="#fff" stroke-width="3.2" stroke-linecap="round" /></svg>
          <h1>{cfg?.company || 'Hardy RMM'}</h1>
          <p class="muted">Remote monitoring &amp; management</p>
        </div>
        {err && <ErrorBox msg={err} />}
        {cfg?.sso && (
          <a class="btn btn-primary btn-md btn-block" href={'/auth/login?return=' + encodeURIComponent(ret)}>
            <Icon name="shield" /> <span>Sign in with single sign-on</span>
          </a>
        )}
        {cfg?.local && cfg?.sso && !showLocal && (
          <button class="linklike" onClick={() => setShowLocal(true)}>Use local admin account</button>
        )}
        {cfg?.local && showLocal && (
          <form onSubmit={submit} class="stack">
            {cfg?.sso && <div class="divider"><span>break-glass login</span></div>}
            <input type="email" placeholder="Email" autoComplete="username" value={email} onInput={(e) => setEmail((e.target as HTMLInputElement).value)} required />
            <input type="password" placeholder="Password" autoComplete="current-password" value={password} onInput={(e) => setPassword((e.target as HTMLInputElement).value)} required />
            <Button variant={cfg?.sso ? 'default' : 'primary'} class="btn-block" loading={busy} onClick={submit as any}>Sign in</Button>
          </form>
        )}
      </div>
    </div>
  );
}
