import { useEffect, useRef, useState } from 'preact/hooks';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { wsUrl } from '../api';
import { Icon } from '../icons';
import { Link } from '../router';
import { Button, Spinner, deviceName, useFetch } from '../ui';

const shells: Record<string, [string, string][]> = {
  windows: [['powershell', 'PowerShell'], ['pwsh', 'PowerShell 7'], ['cmd', 'Command Prompt']],
  darwin: [['zsh', 'zsh'], ['bash', 'bash']],
  linux: [['bash', 'bash'], ['sh', 'sh'], ['zsh', 'zsh']],
};

export function TerminalView({ id, os }: { id: string; os: string }) {
  const host = useRef<HTMLDivElement>(null);
  const [shell, setShell] = useState((shells[os] || shells.linux)[0][0]);
  const [state, setState] = useState<'connecting' | 'ready' | 'closed'>('connecting');
  const [gen, setGen] = useState(0);

  useEffect(() => {
    if (!host.current) return;
    const term = new Terminal({
      cursorBlink: true,
      fontFamily: '"JetBrains Mono", "Cascadia Mono", Menlo, Consolas, monospace',
      fontSize: 13,
      theme: { background: '#0b1220', foreground: '#e2e8f0', cursor: '#60a5fa', selectionBackground: '#334155' },
      scrollback: 5000,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host.current);
    fit.fit();
    setState('connecting');
    const ws = new WebSocket(wsUrl(`/ws/terminal/${id}?shell=${shell}&cols=${term.cols}&rows=${term.rows}`));
    ws.binaryType = 'arraybuffer';
    const dec = new TextDecoder();
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        const m = JSON.parse(ev.data);
        if (m.t === 'ready') { setState('ready'); term.focus(); }
        if (m.t === 'error') term.write(`\r\n\x1b[31m${m.error}\x1b[0m\r\n`);
        if (m.t === 'exit') term.write(`\r\n\x1b[33m[${m.reason}]\x1b[0m\r\n`);
      } else {
        term.write(dec.decode(new Uint8Array(ev.data), { stream: true }));
      }
    };
    ws.onclose = () => setState('closed');
    const d1 = term.onData((data) => ws.readyState === 1 && ws.send(JSON.stringify({ t: 'i', d: data })));
    const d2 = term.onResize(({ cols, rows }) => ws.readyState === 1 && ws.send(JSON.stringify({ t: 'r', cols, rows })));
    const ro = new ResizeObserver(() => { try { fit.fit(); } catch {} });
    ro.observe(host.current);
    return () => {
      ro.disconnect();
      d1.dispose();
      d2.dispose();
      ws.close();
      term.dispose();
    };
  }, [id, shell, gen]);

  return (
    <div class="terminal">
      <div class="term-bar">
        <select value={shell} onChange={(e) => setShell((e.target as HTMLSelectElement).value)}>
          {(shells[os] || shells.linux).map(([v, l]) => <option key={v} value={v}>{l}</option>)}
        </select>
        <span class={'term-state ' + state}><span class="dot" />{state === 'ready' ? 'Connected' : state === 'connecting' ? 'Connecting…' : 'Disconnected'}</span>
        <div class="grow" />
        {state === 'closed' && <Button size="sm" icon="refresh" onClick={() => setGen(gen + 1)}>Reconnect</Button>}
        <Link href={`/devices/${id}/terminal`} class="btn btn-ghost btn-sm" title="Full page"><Icon name="maximize" size={15} /></Link>
      </div>
      <div class="term-host" ref={host} />
    </div>
  );
}

export function TerminalPage({ id }: { id: string }) {
  const { data } = useFetch<any>('/devices/' + id);
  if (!data) return <div class="page"><Spinner /></div>;
  return (
    <div class="page page-full">
      <div class="page-head">
        <div>
          <Link href={`/devices/${id}`} class="back"><Icon name="chevronLeft" size={16} /> {deviceName(data)}</Link>
          <h1>Terminal</h1>
        </div>
      </div>
      <div class="card term-card full"><TerminalView id={id} os={data.os} /></div>
    </div>
  );
}
