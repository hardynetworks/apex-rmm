import { useEffect, useRef, useState } from 'preact/hooks';
import { api, wsUrl } from '../api';
import { Icon } from '../icons';
import { Button, Modal, deviceName, useFetch } from '../ui';
import { TouchControls, TouchHelp, TouchMode, isTouchDevice, loadTouchMode } from './RemoteTouch';
import { native } from '../native';
import { FileLink, FilesPanel, droppedItems, useTransfers } from './RemoteFiles';
import { RTCLink } from './RemoteRTC';
import { StreamSettings, VideoSink, h264Supported, loadSettings, optMessage, saveSettings } from './RemoteVideo';

type Display = { name: string; w: number; h: number; primary: boolean };

type AgentStats = { mode: string; codec: string; fps: number; kbps: number; target: number; rtt: number; cap: number; enc: number; capture: string; congested: boolean };

const FPS_OPTIONS = [15, 30, 60, 120];
const BW_OPTIONS: [number, string][] = [[0, 'Automatic'], [1000, '1 Mbit/s'], [2000, '2 Mbit/s'], [5000, '5 Mbit/s'], [10000, '10 Mbit/s'], [20000, '20 Mbit/s'], [50000, '50 Mbit/s']];
const SCALE_OPTIONS: [number, string][] = [[1, 'Full'], [0.75, '75%'], [0.5, '50%']];

export function RemoteDesktop({ id }: { id: string }) {
  const dev = useFetch<any>('/devices/' + id);
  const canvas = useRef<HTMLCanvasElement>(null);
  const stage = useRef<HTMLDivElement>(null);
  const touch = useRef(isTouchDevice()).current;
  const [touchMode, setTouchMode] = useState<TouchMode>(loadTouchMode);
  const [help, setHelp] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);
  const sinkRef = useRef<VideoSink | null>(null);
  const rtcRef = useRef<RTCLink | null>(null);
  const [path, setPath] = useState<'connecting' | 'direct' | 'relay'>('relay');
  const size = useRef({ w: 0, h: 0 });
  const [status, setStatus] = useState<'starting' | 'connecting' | 'live' | 'ended'>('starting');
  const [error, setError] = useState('');
  const [warning, setWarning] = useState('');
  const [displays, setDisplays] = useState<Display[]>([]);
  const [display, setDisplay] = useState(0);
  const [settings, setSettingsState] = useState<StreamSettings>(loadSettings);
  const settingsRef = useRef(settings);
  settingsRef.current = settings;
  const [showSettings, setShowSettings] = useState(false);
  const [agentStats, setAgentStats] = useState<AgentStats | null>(null);
  const [notice, setNotice] = useState('');
  const vcRef = useRef(false);
  const [link, setLink] = useState<FileLink | null>(null);
  const [filesOpen, setFilesOpen] = useState(false);
  const [dragOver, setDragOver] = useState(false);
  const transfers = useTransfers(link);
  const linkRef = useRef(link);
  linkRef.current = link;
  const remoteOs = useRef('');
  const [toasts, setToasts] = useState<{ id: number; text: string }[]>([]);
  const toast = (text: string, ms = 5000) => {
    const id = Math.random();
    setToasts((t) => [...t.slice(-2), { id, text }]);
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), ms);
  };
  const pendingClip = useRef<string | null>(null);
  // Remote → local clipboard. Browsers only allow it while the page has focus, so a copy that
  // arrives while you're elsewhere is written on your next click or key press here.
  const writeClip = (text: string) => {
    if (!settingsRef.current.clip || !navigator.clipboard?.writeText) return;
    navigator.clipboard.writeText(text).then(() => { pendingClip.current = null; }).catch(() => { pendingClip.current = text; });
  };
  useEffect(() => {
    const flush = () => { const t = pendingClip.current; if (t != null) writeClip(t); };
    window.addEventListener('pointerdown', flush, true);
    window.addEventListener('keydown', flush, true);
    window.addEventListener('focus', flush);
    return () => {
      window.removeEventListener('pointerdown', flush, true);
      window.removeEventListener('keydown', flush, true);
      window.removeEventListener('focus', flush);
    };
  }, []);
  const activeTransfers = transfers.filter((t) => t.state === 'active' || t.state === 'waiting').length;
  const [fit, setFit] = useState(true);
  const fitMode = touch || fit;
  const [stats, setStats] = useState({ fps: 0, kbps: 0 });
  const [paste, setPaste] = useState(false);
  const [gen, setGen] = useState(0);
  const [capture, setCapture] = useState(true); // desktop app: send Win / Alt+Tab … to the remote computer

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
      const vc = await h264Supported();
      vcRef.current = vc;
      if (closed) return;
      const ws = new WebSocket(wsUrl(`/ws/desktop/${sid}`));
      ws.binaryType = 'arraybuffer';
      wsRef.current = ws;
      let queue = Promise.resolve();
      let files: FileLink | null = null;
      const video = new VideoSink(
        () => canvas.current,
        (seq) => { frames++; if (ws.readyState === 1) ws.send(`{"t":"vack","i":${seq}}`); },
        () => { if (ws.readyState === 1) ws.send('{"t":"key"}'); },
      );
      sinkRef.current = video;
      const rtc = new RTCLink(
        (m) => { if (ws.readyState === 1) ws.send(JSON.stringify(m)); },
        () => canvas.current,
        () => { frames++; },
        (p) => setPath(p),
      );
      rtcRef.current = rtc;
      ws.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          const m = JSON.parse(ev.data);
          if (files && files.handle(m)) return;
          if (rtc.handle(m)) return;
          if (m.t === 'clip') {
            writeClip(m.text || '');
            return;
          }
          if (m.t === 'hello') {
            remoteOs.current = m.os || '';
            setDisplays(m.displays || []);
            setDisplay(m.display || 0);
            setWarning(m.warning || '');
            ws.send(JSON.stringify(optMessage(settingsRef.current, vc)));
            if (m.files && !files) {
              files = new FileLink(ws);
              setLink(files);
            }
            setStatus('live');
            const st = settingsRef.current;
            if (vc && st.mode === 'auto' && st.direct && RTCLink.supported() && !rtc.isLive) rtc.start().catch(() => setPath('relay'));
          } else if (m.t === 'size') {
            size.current = { w: m.w, h: m.h };
            const c = canvas.current!;
            c.width = m.w;
            c.height = m.h;
            if (m.video) video.reset();
          } else if (m.t === 'stats') {
            setAgentStats(m);
          } else if (m.t === 'notice') {
            setNotice(m.text);
          } else if (m.t === 'error') {
            setError(m.error);
          }
          return;
        }
        bytes += ev.data.byteLength;
        const buf = ev.data as ArrayBuffer;
        if (files && files.chunk(buf)) return;
        if (new Uint8Array(buf, 0, 1)[0] === 3) {
          video.push(buf);
          return;
        }
        // decode sequentially so tiles land in order
        queue = queue.then(() => drawFrame(buf)).then(() => {
          frames++;
          if (ws.readyState === 1) ws.send('{"t":"ack"}');
        }).catch(() => {});
      };
      ws.onclose = () => {
        rtc.destroy();
        files?.close();
        video.close();
        if (!closed) setStatus('ended');
      };
    })();

    // Protocol 1: 0x01 | u16 n | n * (u16 x,y,w,h | u32 len | jpeg)
    // Protocol 2: 0x02 | u16 n | n * (u8 kind | u16 x,y,w,h | kind 0: u32 len | jpeg; kind 1: r,g,b;
    //             kind 2: u16 srcX,srcY — copy from the screen as it was before this message, i.e. scrolling)
    const drawFrame = async (buf: ArrayBuffer) => {
      const v = new DataView(buf);
      const ver = v.getUint8(0);
      if (ver !== 1 && ver !== 2) return;
      const n = v.getUint16(1);
      let off = 3;
      const tiles: { x: number; y: number; blob: Blob }[] = [];
      const fills: { x: number; y: number; w: number; h: number; c: string }[] = [];
      const copies: { x: number; y: number; w: number; h: number; sx: number; sy: number }[] = [];
      for (let i = 0; i < n; i++) {
        const kind = ver === 2 ? v.getUint8(off++) : 0;
        const x = v.getUint16(off), y = v.getUint16(off + 2), w = v.getUint16(off + 4), h = v.getUint16(off + 6);
        off += 8;
        if (kind === 1) {
          fills.push({ x, y, w, h, c: `rgb(${v.getUint8(off)},${v.getUint8(off + 1)},${v.getUint8(off + 2)})` });
          off += 3;
          continue;
        }
        if (kind === 2) {
          copies.push({ x, y, w, h, sx: v.getUint16(off), sy: v.getUint16(off + 2) });
          off += 4;
          continue;
        }
        const len = v.getUint32(off);
        off += 4;
        tiles.push({ x, y, blob: new Blob([new Uint8Array(buf, off, len)], { type: 'image/jpeg' }) });
        off += len;
      }
      const c = canvas.current;
      const before = copies.length && c ? await createImageBitmap(c) : null;
      const bitmaps = await Promise.all(tiles.map((t) => createImageBitmap(t.blob)));
      const ctx = c?.getContext('2d');
      if (!ctx) return;
      if (before) {
        for (const k of copies) ctx.drawImage(before, k.sx, k.sy, k.w, k.h, k.x, k.y, k.w, k.h);
        before.close();
      }
      for (const f of fills) {
        ctx.fillStyle = f.c;
        ctx.fillRect(f.x, f.y, f.w, f.h);
      }
      bitmaps.forEach((b, i) => {
        ctx.drawImage(b, tiles[i].x, tiles[i].y);
        b.close();
      });
    };

    return () => {
      closed = true;
      setLink(null);
      rtcRef.current?.destroy();
      rtcRef.current = null;
      clearInterval(statTimer);
      wsRef.current?.close();
      wsRef.current = null;
    };
  }, [id, gen]);

  // ----- input -----
  const send = (m: any) => {
    if (rtcRef.current?.sendInput(m)) return; // direct path: skip the server
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
    // Ctrl/Cmd+V: let the browser fire its paste event so we get this computer's clipboard
    // (text goes to the remote clipboard first, files are uploaded), then press V remotely.
    let pasteTimer = 0;
    const kd = (e: KeyboardEvent) => {
      if (document.activeElement !== c) return;
      if (settingsRef.current.clip && (e.ctrlKey || e.metaKey) && !e.altKey && e.code === 'KeyV') {
        clearTimeout(pasteTimer);
        pasteTimer = window.setTimeout(() => { pasteTimer = 0; sendPasteKeys(); }, 250);
        return;
      }
      e.preventDefault();
      send({ t: 'kd', code: e.code });
    };
    const onPaste = (e: ClipboardEvent) => {
      if (document.activeElement !== c || !settingsRef.current.clip) return;
      e.preventDefault();
      if (!pasteTimer) return; // not from our Ctrl+V (e.g. a context menu): ignore
      clearTimeout(pasteTimer);
      pasteTimer = 0;
      const files = Array.from(e.clipboardData?.files || []);
      if (files.length && linkRef.current) {
        pasteFiles(files);
        return;
      }
      const text = e.clipboardData?.getData('text/plain') || '';
      if (text) send({ t: 'clip', text });
      sendPasteKeys();
    };
    const ku = (e: KeyboardEvent) => {
      if (document.activeElement !== c) return;
      e.preventDefault();
      if (e.code === 'KeyV' && (e.ctrlKey || e.metaKey) && settingsRef.current.clip) return; // sent with the paste
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
    window.addEventListener('paste', onPaste);
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
      window.removeEventListener('paste', onPaste);
      clearTimeout(pasteTimer);
    };
  }, [status]);

  // V with the paste modifier the remote computer expects (Cmd on a Mac, Ctrl elsewhere).
  const sendPasteKeys = () => {
    const localMac = /Mac|iPhone|iPad/.test(navigator.platform);
    const remoteMac = remoteOs.current === 'darwin';
    const seq: [string, string][] = localMac && !remoteMac
      ? [['ku', 'MetaLeft'], ['kd', 'ControlLeft'], ['kd', 'KeyV'], ['ku', 'KeyV'], ['ku', 'ControlLeft']]
      : !localMac && remoteMac
        ? [['ku', 'ControlLeft'], ['kd', 'MetaLeft'], ['kd', 'KeyV'], ['ku', 'KeyV'], ['ku', 'MetaLeft']]
        : [['kd', 'KeyV'], ['ku', 'KeyV']];
    seq.forEach(([t, code]) => send({ t, code }));
  };

  // Files pasted with Ctrl+V: into the Explorer/Finder folder that has focus on the remote
  // computer, or into the focused program through the remote clipboard.
  const pasteFiles = async (files: File[]) => {
    const l = linkRef.current;
    if (!l) return;
    try {
      const target = await l.request({ t: 'fdrop', fg: true, x: 0, y: 0 });
      toast(`Copying ${files.length === 1 ? files[0].name : files.length + ' files'} to ${target.label}…`);
      const paths = await l.uploadTree(files.map((file) => ({ file, rel: file.name })), target.path);
      if (target.kind === 'app' && remoteOs.current === 'windows' && paths.length) {
        await l.request({ t: 'fclip', paths, paste: true });
        toast(`Pasted into ${target.label} (also saved in Downloads)`);
      } else {
        toast(`Copied to ${target.path}`);
      }
    } catch (err: any) {
      toast(`Paste failed: ${err.message}`);
    }
  };

  // Desktop app: it captures shortcuts Windows would otherwise keep (Win, Alt+Tab, Ctrl+Esc …)
  // and hands them to us here.
  useEffect(() => {
    if (!native || status !== 'live') return;
    (window as any).__apexKey = (t: 'kd' | 'ku', code: string) => send({ t, code });
    native.setCapture(true).then((on) => setCapture(on)).catch(() => {});
    return () => {
      native!.setCapture(false).catch(() => {});
      delete (window as any).__apexKey;
    };
  }, [status]);
  const toggleCapture = () => {
    const on = !capture;
    setCapture(on);
    native?.setCaptureDefault(on).catch(() => {});
  };

  useEffect(() => {
    if (!touch || status !== 'live') return;
    try {
      if (!localStorage.getItem('apex-rd-touch-help')) { setHelp(true); localStorage.setItem('apex-rd-touch-help', '1'); }
    } catch {}
  }, [status]);

  const updateSettings = (patch: Partial<StreamSettings>) => {
    const s = { ...settingsRef.current, ...patch };
    setSettingsState(s);
    saveSettings(s);
    send(optMessage(s, vcRef.current));
    if ('direct' in patch) {
      if (s.direct && vcRef.current && RTCLink.supported()) rtcRef.current?.start().catch(() => setPath('relay'));
      else { rtcRef.current?.stop(); setPath('relay'); }
    }
  };
  // Drag files from this computer onto the remote screen to upload them.
  // Files and folders land where they're dropped: in the Explorer/Finder folder under the
  // pointer, on the desktop, or (over another program) in Downloads and on the remote clipboard.
  const hasFiles = (e: DragEvent) => !!link && !!e.dataTransfer?.types.includes('Files');
  const dragMove = useRef(0);
  const stageDragOver = (e: DragEvent) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
    setDragOver(true);
    const now = performance.now();
    if (now - dragMove.current > 60) { // the remote pointer follows, so you can see where it'll land
      dragMove.current = now;
      send({ t: 'mm', ...pos(e) });
    }
  };
  const stageDrop = (e: DragEvent) => {
    if (!hasFiles(e) || !link) return;
    e.preventDefault();
    setDragOver(false);
    const p = pos(e);
    send({ t: 'mm', ...p });
    const l = link;
    droppedItems(e.dataTransfer!).then(async (items) => {
      if (!items.length) return;
      const target = await l.request({ t: 'fdrop', x: p.x, y: p.y });
      const what = items.length === 1 ? items[0].file.name : items.some((i) => i.rel.includes('/')) ? 'the dropped items' : items.length + ' files';
      toast(`Copying ${what} to ${target.kind === 'app' ? 'Downloads' : target.label}…`);
      const paths = await l.uploadTree(items, target.path);
      if (target.kind === 'app' && remoteOs.current === 'windows' && paths.length) {
        await l.request({ t: 'fclip', paths }).catch(() => {});
        toast(`Saved to Downloads and copied to the remote clipboard: click where you want them in ${target.label} and press Ctrl+V.`, 9000);
      } else {
        toast(`Copied to ${target.path}`);
      }
    }).catch((err) => toast(`Upload failed: ${err.message}`));
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
          {status === 'live' && (
            <button class="rd-statline mono" onClick={() => setShowSettings(!showSettings)} title="Stream details and settings">
              {agentStats?.mode === 'h264' ? 'H.264' : 'Images'}{path === 'direct' ? ' · direct' : ''} · {stats.fps} fps · {fmtRate(stats.kbps)}{agentStats && agentStats.rtt > 0 ? ` · ${agentStats.rtt} ms` : ''}
            </button>
          )}
        </div>
        <div class="rd-tools">
          {displays.length > 1 && (
            <select value={display} onChange={(e) => pickDisplay(+(e.target as HTMLSelectElement).value)} title="Display">
              {displays.map((d, i) => <option key={i} value={i}>{d.name}{d.primary ? ' (primary)' : ''}</option>)}
            </select>
          )}
          {link && (
            <Button size="sm" variant="ghost" icon="folder" class={filesOpen ? 'rd-capture on' : ''} onClick={() => setFilesOpen(!filesOpen)} title="Transfer files to and from the remote device">
              {touch ? (activeTransfers ? String(activeTransfers) : '') : activeTransfers ? `Files (${activeTransfers})` : 'Files'}
            </Button>
          )}
          <Button size="sm" variant="ghost" icon="settings" class={showSettings ? 'rd-capture on' : ''} onClick={() => setShowSettings(!showSettings)} title="Frame rate, bandwidth and resolution">{touch ? '' : 'Stream'}</Button>
          {!touch && <Button size="sm" variant="ghost" icon={fit ? 'maximize' : 'devices'} onClick={() => setFit(!fit)}>{fit ? 'Fit' : '1:1'}</Button>}
          {!touch && <Button size="sm" variant="ghost" icon="keyboard" onClick={() => send({ t: 'cad' })} title="Send Ctrl+Alt+Del">Ctrl+Alt+Del</Button>}
          <Button size="sm" variant="ghost" icon="copy" onClick={() => setPaste(true)} title="Type text on the remote device">{touch ? '' : 'Paste text'}</Button>
          {touch && <Button size="sm" variant="ghost" onClick={() => setHelp(true)} title="Touch gestures">?</Button>}
          {native && (
            <Button size="sm" variant="ghost" icon="keyboard" class={capture ? 'rd-capture on' : 'rd-capture'} onClick={toggleCapture}
              title={capture ? 'Windows shortcuts (Win key, Alt+Tab, Ctrl+Esc, Alt+F4) go to the remote computer. Click to keep them on this PC.' : 'Windows shortcuts stay on this PC. Click to send them to the remote computer.'}>
              {capture ? 'Shortcuts: remote' : 'Shortcuts: local'}
            </Button>
          )}
          <Button size="sm" variant="ghost" icon="refresh" onClick={() => send({ t: 'refresh' })} title="Refresh screen" />
          <Button size="sm" variant="ghost" icon="maximize" onClick={fullscreen} title="Full screen" />
          {status === 'ended'
            ? <Button size="sm" variant="primary" icon="refresh" onClick={() => setGen(gen + 1)}>Reconnect</Button>
            : <Button size="sm" variant="danger" icon="x" onClick={() => { wsRef.current?.close(); window.close(); }}>End</Button>}
        </div>
      </header>
      {warning && <div class="rd-warn"><Icon name="alert" size={16} /> {warning}</div>}
      {toasts.length > 0 && <div class="rd-toasts">{toasts.map((t) => <div key={t.id} class="rd-toast">{t.text}</div>)}</div>}
      {notice && <div class="rd-warn"><Icon name="info" size={16} /> {notice} <button class="rd-link" onClick={() => setNotice('')}>Dismiss</button></div>}
      {showSettings && (
        <StreamPanel settings={settings} stats={agentStats} path={path} viewerFps={stats.fps} vc={vcRef.current} onChange={updateSettings} onClose={() => { setShowSettings(false); canvas.current?.focus(); }} />
      )}
      <div class="rd-main">
      <div ref={stage} class={'rd-stage ' + (fitMode ? 'fit' : 'actual') + (touch ? ' touch' : '')}
        onDragOver={stageDragOver} onDragLeave={() => setDragOver(false)} onDrop={stageDrop}>
        <canvas ref={canvas} tabIndex={0} class={status === 'live' ? '' : 'hidden'} />
        {dragOver && <div class="rd-drop"><Icon name="upload" size={32} /><span>Drop to copy here on the remote computer<br /><small>into the folder or desktop under the pointer</small></span></div>}
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
      {filesOpen && link && <FilesPanel link={link} onClose={() => { setFilesOpen(false); canvas.current?.focus(); }} />}
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

function fmtRate(kbps: number) {
  return kbps >= 1000 ? (kbps / 1000).toFixed(1) + ' Mbit/s' : Math.round(kbps) + ' kbit/s';
}

function StreamPanel({ settings, stats, path, viewerFps, vc, onChange, onClose }: {
  settings: StreamSettings; stats: AgentStats | null; path: string; viewerFps: number; vc: boolean;
  onChange: (p: Partial<StreamSettings>) => void; onClose: () => void;
}) {
  const video = stats?.mode === 'h264';
  let why = '';
  if (settings.mode === 'images') why = 'Images mode selected.';
  else if (!vc) why = "This browser can't decode H.264 video (WebCodecs), so images are used.";
  else if (stats && !video) why = stats.codec === 'downloading' ? 'The device is downloading the video encoder; video starts automatically when it is ready.' : `Video unavailable on the device: ${stats.codec || 'starting'}.`;
  return (
    <div class="rd-stream" onKeyDown={(e) => e.stopPropagation()}>
      <div class="rd-stream-head">
        <strong>Stream</strong>
        <button class="rd-icon-btn" onClick={onClose} title="Close"><Icon name="x" size={15} /></button>
      </div>
      <label>Frame rate
        <div class="rd-seg">
          {FPS_OPTIONS.map((f) => <button key={f} class={settings.fps === f ? 'on' : ''} onClick={() => onChange({ fps: f })}>{f}</button>)}
        </div>
      </label>
      <label>Bandwidth
        <select value={settings.bw} onChange={(e) => onChange({ bw: +(e.target as HTMLSelectElement).value })}>
          {BW_OPTIONS.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
        </select>
      </label>
      <label>Resolution
        <div class="rd-seg">
          {SCALE_OPTIONS.map(([v, l]) => <button key={v} class={settings.scale === v ? 'on' : ''} onClick={() => onChange({ scale: v })}>{l}</button>)}
        </div>
      </label>
      <label>Encoding
        <div class="rd-seg">
          <button class={settings.mode === 'auto' ? 'on' : ''} onClick={() => onChange({ mode: 'auto' })}>Video (H.264)</button>
          <button class={settings.mode === 'images' ? 'on' : ''} onClick={() => onChange({ mode: 'images' })}>Images</button>
        </div>
      </label>
      <label>Clipboard
        <div class="rd-seg">
          <button class={settings.clip ? 'on' : ''} onClick={() => onChange({ clip: true })}>Shared</button>
          <button class={!settings.clip ? 'on' : ''} onClick={() => onChange({ clip: false })}>Off</button>
        </div>
      </label>
      <label>Connection
        <div class="rd-seg">
          <button class={settings.direct ? 'on' : ''} onClick={() => onChange({ direct: true })}>Direct when possible</button>
          <button class={!settings.direct ? 'on' : ''} onClick={() => onChange({ direct: false })}>Via server</button>
        </div>
      </label>
      {why && <p class="rd-stream-why">{why}</p>}
      {stats && (
        <table class="rd-stream-stats">
          <tbody>
            <tr><td>Showing</td><td>{viewerFps} fps (device sends {stats.fps.toFixed(0)})</td></tr>
            <tr><td>Bandwidth</td><td>{fmtRate(stats.kbps)}{video ? ` (target ${fmtRate(stats.target)}${stats.congested ? ', reduced: the connection is busy' : ''})` : ''}</td></tr>
            <tr><td>Path</td><td>{path === 'direct' ? 'Direct (WebRTC)' : path === 'connecting' ? 'Trying a direct connection…' : 'Via the Apex server'}</td></tr>
            <tr><td>Round trip</td><td>{stats.rtt ? stats.rtt + ' ms' : '–'}</td></tr>
            <tr><td>Capture</td><td>{stats.cap.toFixed(1)} ms ({stats.capture})</td></tr>
            <tr><td>Encode</td><td>{stats.enc.toFixed(1)} ms</td></tr>
          </tbody>
        </table>
      )}
      <p class="rd-stream-why">Automatic bandwidth lowers quality when the connection gets busy and raises it again when there's room. A still screen sends almost nothing, so the frame rate drops when nothing moves.</p>
    </div>
  );
}
