import { useEffect, useState } from 'preact/hooks';
import { api } from '../api';
import { navigate } from '../router';
import { Button, ErrorBox, Field, Spinner } from '../ui';

/** First-run wizard: creates the first admin and the basic server settings. */
export function Setup() {
  const [status, setStatus] = useState<any>(null);
  const [step, setStep] = useState(1);
  const [v, setV] = useState({ code: '', company_name: 'Hardy RMM', public_url: location.origin, name: '', email: '', password: '', password2: '' });
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const set = (k: string, val: string) => setV({ ...v, [k]: val });

  useEffect(() => {
    api.get('/setup').then((s) => {
      if (!s.needed) navigate('/login', true);
      setStatus(s);
      if (s.public_url_locked) setV((x) => ({ ...x, public_url: s.public_url }));
    });
  }, []);

  if (!status) return <div class="boot"><Spinner /></div>;

  const next = (e: Event) => {
    e.preventDefault();
    setErr('');
    if (step === 1 && !v.code.trim()) return setErr('Enter the setup code from the server log.');
    if (step === 2 && !/^https?:\/\/[^/]+/.test(v.public_url)) return setErr('The public URL must look like https://rmm.example.com');
    setStep(step + 1);
  };
  const finish = async (e: Event) => {
    e.preventDefault();
    setErr('');
    if (v.password !== v.password2) return setErr('The passwords do not match.');
    if (v.password.length < 10) return setErr('Use at least 10 characters for the password.');
    setBusy(true);
    try {
      await api.post('/setup', v);
      location.href = '/settings?tab=general';
    } catch (e: any) {
      setErr(e.message);
      if (/setup code/i.test(e.message)) setStep(1);
    }
    setBusy(false);
  };

  return (
    <div class="login">
      <div class="login-card setup-card">
        <div class="login-brand">
          <svg viewBox="0 0 32 32" width="44" height="44"><rect width="32" height="32" rx="8" fill="var(--accent)" /><path d="M9 8v16M23 8v16M9 16h14" stroke="#fff" stroke-width="3.2" stroke-linecap="round" /></svg>
          <h1>Welcome to Hardy RMM</h1>
          <p class="muted">Let's get your server set up.</p>
        </div>
        <div class="steps">{[1, 2, 3].map((i) => <span key={i} class={i <= step ? 'on' : ''} />)}</div>
        {err && <ErrorBox msg={err} />}

        {step === 1 && (
          <form onSubmit={next} class="stack">
            <p class="small">To prove you own this server, enter the setup code printed in its log:</p>
            <pre class="output">docker compose logs hardy | grep "setup code"</pre>
            <Field label="Setup code"><input value={v.code} autoFocus autoComplete="off" class="mono" onInput={(e) => set('code', (e.target as HTMLInputElement).value)} /></Field>
            <Button variant="primary" class="btn-block" onClick={next as any}>Continue</Button>
          </form>
        )}

        {step === 2 && (
          <form onSubmit={next} class="stack">
            <Field label="Company name" hint="Shown in the sidebar and used for your first client."><input value={v.company_name} onInput={(e) => set('company_name', (e.target as HTMLInputElement).value)} /></Field>
            <Field label="Public URL" hint={status.public_url_locked ? 'Set by PUBLIC_URL in .env.' : 'The address people and agents use to reach this server. Usually the address in your browser right now.'}>
              <input value={v.public_url} disabled={status.public_url_locked} onInput={(e) => set('public_url', (e.target as HTMLInputElement).value)} />
            </Field>
            <div class="row-fields">
              <Button onClick={() => setStep(1)}>Back</Button>
              <Button variant="primary" class="grow" onClick={next as any}>Continue</Button>
            </div>
          </form>
        )}

        {step === 3 && (
          <form onSubmit={finish} class="stack">
            <p class="small muted">Create the first administrator. This local login keeps working even if single sign-on is down.</p>
            <Field label="Your name"><input value={v.name} autoFocus onInput={(e) => set('name', (e.target as HTMLInputElement).value)} /></Field>
            <Field label="Email"><input type="email" autoComplete="username" value={v.email} onInput={(e) => set('email', (e.target as HTMLInputElement).value)} /></Field>
            <Field label="Password" hint="At least 10 characters"><input type="password" autoComplete="new-password" value={v.password} onInput={(e) => set('password', (e.target as HTMLInputElement).value)} /></Field>
            <Field label="Repeat password"><input type="password" autoComplete="new-password" value={v.password2} onInput={(e) => set('password2', (e.target as HTMLInputElement).value)} /></Field>
            <div class="row-fields">
              <Button onClick={() => setStep(2)}>Back</Button>
              <Button variant="primary" class="grow" loading={busy} onClick={finish as any}>Create admin &amp; finish</Button>
            </div>
            <p class="muted small">Next you'll land in Settings, where you can connect Authentik and RustDesk.</p>
          </form>
        )}
      </div>
    </div>
  );
}
