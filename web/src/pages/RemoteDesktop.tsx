import { useEffect, useRef, useState } from 'preact/hooks';
import { api, wsUrl } from '../api';
import { Icon } from '../icons';
import { Button, Modal, deviceName, useFetch } from '../ui';
import { TouchControls, TouchHelp, TouchMode, isTouchDevice, loadTouchMode } from './RemoteTouch';

type Display = { name: string; w: number; h: number; primary: boolean };

const QUALITY: Record<string, { q: number; s: number; fps: number }> = {
  low: { q: 35, s: 0.5, fps: 10 },
  balanced: { q: 60, s: 0.75, fps: 15 },
  high: { q: 80, s: 1, fps: 20 },
};

export function RemoteDesktop({ id }: { id: string }) {
  const dev = useFetch<any>('/devices/' + id);
  const canvas = useRef<HTMLCanvasElement>(null);
  const stage = useRef<HTMLDivElement>(null);
  const touch = useRef(isTouchDevice()).current;
  const [touchMode, setTouchMode] = useState<TouchMode>(loadTouchMode);
  const [help, setHelp] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const size = useRef({ w: 0, h: 0 });
  const [status, setStatus] = useState<'starting' | 'connecting' | 'live' | 'ended'>('starting');
  const [error, setError] = useState('');
  const [warning, setWarning] = useState('');
  const [displays, setDisplays] = useState<Display[]>([]);
  const [display, setDisplay] = useState(0);
  const [quality, setQuality] = useState('balanced');
  const [fit, setFit] = useState(true);
  const fitMode = touch || fit;
  const [stats, setStats] = useState({ fps: 0, kbps: 0 });
  const [paste, setPaste] = useState(false);
  const [gen, setGen] = useState(0);

  useEffect(() => {
    if (dev.data) document.title = `${deviceName(dev.data)} — Remote control`;
  }, [dev.data]);

  useEffect(() => {
    let closed = false;
    let frames = 0;
    let bytes = 0;
    setError('');
    setStatus('starting');
    const statTimer = setInterval(() => {
      setStats({ fps: frames, kbps: Math.round((bytes * 8) / 1000) });
      frames = 0;
      bytes = 0;
    }, 1000);

    (async () => {
      let sid: string;
      try {
        sid = (await api.post(`/devices/${id}/desktop`)).session_id;
      } catch (e: any) {
        setError(e.message);
        setStatus('ended');
        return;
      }
      if (closed) return;
      setStatus('connecting');
      const ws = new WebSocket(wsUrl(`/ws/desktop/${sid}`));
      ws.binaryType = 'arraybuffer';
      wsRef.current = ws;
      let queue = Promise.resolve();
      ws.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          const m = JSON.parse(ev.data);
          if (m.t === 'hello') {
            setDisplays(m.displays || []);
            setDisplay(m.display || 0);
            setWarning(m.warning || '');
            const qv = QUALITY[quality];
            ws.send(JSON.stringify({ t: 'opt', ...qv }));
            setStatus('live');
          } else if (m.t === 'size') {
            size.current = { w: m.w, h: m.h };
            const c = canvas.current!;
            c.width = m.w;
            c.height = m.h;
          } else if (m.t === 'error') {
            setError(m.error);
          }
          return;
        }
        bytes += ev.data.byteLength;
        const buf = ev.data as ArrayBuffer;
        // decode sequentially so tiles land in order
        queue = queue.then(() => drawFrame(buf)).then(() => {
          frames++;
          if (ws.readyState === 1) ws.send('{"t":"ack"}');
        }).catch(() => {});
      };
      ws.onclose = () => {
        if (!closed) setStatus('ended');
      };
    })();

    const drawFrame = async (buf: ArrayBuffer) => {
      const v = new DataView(buf);
      if (v.getUint8(0) !== 1) return;
      const n = v.getUint16(1);
      let off = 3;
      const tiles: { x: number; y: number; blob: Blob }[] = [];
      for (let i = 0; i < n; i++) {
        const x = v.getUint16(off), y = v.getUint16(off + 2);
        const len = v.getUint32(off + 8);
        off += 12;
        tiles.push({ x, y, blob: new Blob([new Uint8Array(buf, off, len)], { type: 'image/jpeg' }) });
        off += len;
      }
      const bitmaps = await Promise.all(tiles.map((t) => createImageBitmap(t.blob)));
      const ctx = canvas.current?.getContext('2d');
      if (!ctx) return;
      bitmaps.forEach((b, i) => {
        ctx.drawImage(b, tiles[i].x, tiles[i].y);
        b.close();
      });
    };

    return () => {
      closed = true;
      clearInterval(statTimer);
      wsRef.current?.close();
      wsRef.current = null;
    };
  }, [id, gen]);

  // ----- input -----
  const send = (m: any) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === 1) ws.send(JSON.stringify(m));
  };
  const pos = (e: MouseEvent) => {
    const c = canvas.current!;
    const r = c.getBoundingClientRect();
    // account for object-fit: contain letterboxing
    const scale = Math.min(r.width / c.width, r.height / c.height);
    const dw = c.width * scale, dh = c.height * scale;
    const ox = (r.width - dw) / 2, oy = (r.height - dh) / 2;
    const x = Math.round(((e.clientX - r.left - ox) / dw) * c.width);
    const y = Math.round(((e.clientY - r.top - oy) / dh) * c.height);
    return { x: Math.max(0, Math.min(c.width - 1, x)), y: Math.max(0, Math.min(c.height - 1, y)) };
  };

  useEffect(() => {
    const c = canvas.current;
    if (!c || status !== 'live') return;
    let pending: { x: number; y: number } | null = null;
    let raf = 0;
    const mm = (e: MouseEvent) => {
      pending = pos(e);
      if (!raf) raf = requestAnimationFrame(() => { raf = 0; if (pending) send({ t: 'mm', ...pending }); });
    };
    const md = (e: MouseEvent) => { e.preventDefault(); c.focus(); send({ t: 'md', b: e.button, ...pos(e) }); };
    const mu = (e: MouseEvent) => { e.preventDefault(); send({ t: 'mu', b: e.button, ...pos(e) }); };
    let acc = 0;
    const wh = (e: WheelEvent) => {
      e.preventDefault();
      acc += e.deltaMode === 1 ? e.deltaY * 33 : e.deltaY;
      const notches = Math.trunc(acc / 60);
      if (notches !== 0) { acc -= notches * 60; send({ t: 'wh', dx: 0, dy: Math.max(-5, Math.min(5, notches)) }); }
    };
    const cm = (e: Event) => e.preventDefault();
    const kd = (e: KeyboardEvent) => {
      if (document.activeElement !== c) return;
      e.preventDefault();
      send({ t: 'kd', code: e.code });
    };
    const ku = (e: KeyboardEvent) => {
      if (document.activeElement !== c) return;
      e.preventDefault();
      send({ t: 'ku', code: e.code });
    };
    const blur = () => ['ShiftLeft', 'ShiftRight', 'ControlLeft', 'ControlRight', 'AltLeft', 'AltRight', 'MetaLeft', 'MetaRight'].forEach((code) => send({ t: 'ku', code }));
    c.addEventListener('mousemove', mm);
    c.addEventListener('mousedown', md);
    c.addEventListener('mouseup', mu);
    c.addEventListener('wheel', wh, { passive: false });
    c.addEventListener('contextmenu', cm);
    c.addEventListener('blur', blur);
    window.addEventListener('keydown', kd);
    window.addEventListener('keyup', ku);
    c.focus();
    return () => {
      c.removeEventListener('mousemove', mm);
      c.removeEventListener('mousedown', md);
      c.removeEventListener('mouseup', mu);
      c.removeEventListener('wheel', wh);
      c.removeEventListener('contextmenu', cm);
      c.removeEventListener('blur', blur);
      window.removeEventListener('keydown', kd);
      window.removeEventListener('keyup', ku);
    };
  }, [status]);

  useEffect(() => {
    if (!touch || status !== 'live') return;
    try {
      if (!localStorage.getItem('apex-rd-touch-help')) { setHelp(true); localStorage.setItem('apex-rd-touch-help', '1'); }
    } catch {}
  }, [status]);

  const setQ = (q: string) => {
    setQuality(q);
    send({ t: 'opt', ...QUALITY[q] });
  };
  const pickDisplay = (i: number) => {
    setDisplay(i);
    send({ t: 'display', i });
  };
  const fullscreen = () => {
    if (document.fullscreenElement) document.exitFullscreen();
    else document.documentElement.requestFullscreen().catch(() => {});
  };

  const name = dev.data ? deviceName(dev.data) : '…';
  return (
    <div class="rd">
      <header class="rd-bar">
        <div class="rd-title">
          <Icon name="pointer" size={16} />
          <strong>{name}</strong>
          <span class={'rd-status ' + status}><span class="dot" />{status === 'live' ? 'Live' : status === 'ended' ? 'Disconnected' : 'Connecting…'}</span>
          {status === 'live' && <span class="muted small mono">{stats.fps} fps · {stats.kbps > 1000 ? (stats.kbps / 1000).toFixed(1) + ' Mbps' : stats.kbps + ' kbps'}</span>}
        </div>
        <div class="rd-tools">
          {displays.length > 1 && (
            <select value={display} onChange={(e) => pickDisplay(+(e.target as HTMLSelectElement).value)} title="Display">
              {displays.map((d, i) => <option key={i} value={i}>{d.name}{d.primary ? ' (primary)' : ''}</option>)}
            </select>
          )}
          <select value={quality} onChange={(e) => setQ((e.target as HTMLSelectElement).value)} title="Quality">
            <option value="low">Low bandwidth</option>
            <option value="balanced">Balanced</option>
            <option value="high">High quality</option>
          </select>
          {!touch && <Button size="sm" variant="ghost" icon={fit ? 'maximize' : 'devices'} onClick={() => setFit(!fit)}>{fit ? 'Fit' : '1:1'}</Button>}
          {!touch && <Button size="sm" variant="ghost" icon="keyboard" onClick={() => send({ t: 'cad' })} title="Send Ctrl+Alt+Del">Ctrl+Alt+Del</Button>}
          <Button size="sm" variant="ghost" icon="copy" onClick={() => setPaste(true)} title="Type text on the remote device">{touch ? '' : 'Paste text'}</Button>
          {touch && <Button size="sm" variant="ghost" onClick={() => setHelp(true)} title="Touch gestures">?</Button>}
          <Button size="sm" variant="ghost" icon="refresh" onClick={() => send({ t: 'refresh' })} title="Refresh screen" />
          <Button size="sm" variant="ghost" icon="maximize" onClick={fullscreen} title="Full screen" />
          {status === 'ended'
            ? <Button size="sm" variant="primary" icon="refresh" onClick={() => setGen(gen + 1)}>Reconnect</Button>
            : <Button size="sm" variant="danger" icon="x" onClick={() => { wsRef.current?.close(); window.close(); }}>End</Button>}
        </div>
      </header>
      {warning && <div class="rd-warn"><Icon name="alert" size={16} /> {warning}</div>}
      <div ref={stage} class={'rd-stage ' + (fitMode ? 'fit' : 'actual') + (touch ? ' touch' : '')}>
        <canvas ref={canvas} tabIndex={0} class={status === 'live' ? '' : 'hidden'} />
        {status !== 'live' && (
          <div class="rd-overlay">
            {error ? (
              <div class="rd-msg">
                <Icon name="alert" size={28} />
                <strong>Couldn't start remote control</strong>
                <p>{error}</p>
                <p class="muted small">Tip: RustDesk on the device page works as a backup connection.</p>
                <Button icon="refresh" onClick={() => setGen(gen + 1)}>Try again</Button>
              </div>
            ) : status === 'ended' ? (
              <div class="rd-msg"><strong>Session ended</strong><Button icon="refresh" onClick={() => setGen(gen + 1)}>Reconnect</Button></div>
            ) : (
              <div class="rd-msg"><span class="spinner" /><strong>Starting session…</strong><p class="muted">Launching the capture helper on the device</p></div>
            )}
          </div>
        )}
      </div>
      {touch && status === 'live' && (
        <TouchControls stage={stage} canvas={canvas} send={send} mode={touchMode} setMode={setTouchMode} onCad={() => send({ t: 'cad' })} />
      )}
      {help && <TouchHelp mode={touchMode} onClose={() => setHelp(false)} />}
      {paste && <PasteModal onClose={() => setPaste(false)} onSend={(t) => { send({ t: 'type', text: t }); setPaste(false); canvas.current?.focus(); }} />}
    </div>
  );
}

function PasteModal({ onClose, onSend }: { onClose: () => void; onSend: (t: string) => void }) {
  const [text, setText] = useState('');
  useEffect(() => { navigator.clipboard?.readText?.().then((t) => t && setText(t)).catch(() => {}); }, []);
  return (
    <Modal title="Type text on the remote device" onClose={onClose} width={480}
      footer={<><Button onClick={onClose}>Cancel</Button><Button variant="primary" icon="send" disabled={!text} onClick={() => onSend(text)}>Type it</Button></>}>
      <p class="muted small">The text is typed as keystrokes into the focused window on the device.</p>
      <textarea rows={6} value={text} onInput={(e) => setText((e.target as HTMLTextAreaElement).value)} autoFocus />
    </Modal>
  );
}
