// Touch controls for the remote-control viewer on phones and tablets.
//
// Two input modes:
//   trackpad – the screen works like a laptop trackpad: drag moves an on-screen
//              cursor, tap = left click, two-finger tap = right click,
//              two-finger drag = scroll, double-tap-and-drag (or long press) = drag.
//   touch    – tap where you want to click, long press = right click,
//              drag = click-and-drag, two-finger drag = scroll.
// Both modes: pinch to zoom (and move two fingers to pan while zoomed).
// A bottom bar adds left/right mouse buttons, the phone keyboard and a row of
// special keys (Esc, Tab, Ctrl, Alt, Win, arrows, F-keys …).
import type { RefObject } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import { Icon } from '../icons';

export type TouchMode = 'trackpad' | 'touch';
type Send = (m: any) => void;

const MODE_KEY = 'apex-rd-touch-mode';
const SENTINEL = '  ';
const TAP_MS = 280;
const TAP_SLOP = 10;
const LONG_MS = 550;

// Primary pointer is a finger (phones, tablets). Touchscreen laptops keep the desktop viewer.
export const isTouchDevice = () =>
  typeof window !== 'undefined' && !!window.matchMedia?.('(pointer: coarse)').matches;

export function loadTouchMode(): TouchMode {
  try {
    const v = localStorage.getItem(MODE_KEY);
    if (v === 'touch' || v === 'trackpad') return v;
  } catch {}
  return 'trackpad';
}
function saveTouchMode(m: TouchMode) {
  try { localStorage.setItem(MODE_KEY, m); } catch {}
}

const MODS = [
  { code: 'ControlLeft', label: 'Ctrl' },
  { code: 'AltLeft', label: 'Alt' },
  { code: 'ShiftLeft', label: 'Shift' },
  { code: 'MetaLeft', label: 'Win' },
];
const KEYS: { code: string; label: string }[] = [
  { code: 'Escape', label: 'Esc' },
  { code: 'Tab', label: 'Tab' },
  { code: 'ArrowLeft', label: '←' },
  { code: 'ArrowUp', label: '↑' },
  { code: 'ArrowDown', label: '↓' },
  { code: 'ArrowRight', label: '→' },
  { code: 'Backspace', label: '⌫' },
  { code: 'Delete', label: 'Del' },
  { code: 'Enter', label: 'Enter' },
  { code: 'Home', label: 'Home' },
  { code: 'End', label: 'End' },
  { code: 'PageUp', label: 'PgUp' },
  { code: 'PageDown', label: 'PgDn' },
  ...Array.from({ length: 12 }, (_, i) => ({ code: 'F' + (i + 1), label: 'F' + (i + 1) })),
];
// keys a soft or hardware keyboard sends that we forward as key presses
const SPECIAL = new Set(['Enter', 'Tab', 'Escape', 'Backspace', 'Delete', 'ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End', 'PageUp', 'PageDown']);

function charCode(ch: string): string | null {
  if (/^[a-z]$/i.test(ch)) return 'Key' + ch.toUpperCase();
  if (/^[0-9]$/.test(ch)) return 'Digit' + ch;
  if (ch === ' ') return 'Space';
  return null;
}

export function TouchControls({ stage, canvas, send, mode, setMode, onCad }: {
  stage: RefObject<HTMLDivElement>;
  canvas: RefObject<HTMLCanvasElement>;
  send: Send;
  mode: TouchMode;
  setMode: (m: TouchMode) => void;
  onCad: () => void;
}) {
  const cursorEl = useRef<HTMLDivElement>(null);
  const kbd = useRef<HTMLTextAreaElement>(null);
  const [kbOpen, setKbOpen] = useState(false);
  const [keysOpen, setKeysOpen] = useState(false);
  const [mods, setMods] = useState<string[]>([]);
  const [zoomed, setZoomed] = useState(false);
  const [held, setHeld] = useState<number | null>(null);
  const modsRef = useRef<string[]>([]);
  modsRef.current = mods;
  const modeRef = useRef(mode);
  modeRef.current = mode;

  // shared mutable state (not React state: updated at touch-move rate)
  const st = useRef({
    cx: -1, cy: -1, // cursor in remote pixels
    lw: 0, lh: 0, // frame size the cursor position refers to
    z: 1, tx: 0, ty: 0, // zoom + pan of the canvas
    pending: false,
  }).current;

  // ---------- geometry ----------
  const remote = () => ({ w: canvas.current?.width || 0, h: canvas.current?.height || 0 });
  const content = () => {
    const c = canvas.current!;
    const r = c.getBoundingClientRect();
    const { w, h } = remote();
    const s = Math.min(r.width / w, r.height / h);
    const dw = w * s, dh = h * s;
    return { left: r.left + (r.width - dw) / 2, top: r.top + (r.height - dh) / 2, s };
  };
  const toRemote = (x: number, y: number) => {
    const { w, h } = remote();
    const c = content();
    return {
      x: Math.max(0, Math.min(w - 1, Math.round((x - c.left) / c.s))),
      y: Math.max(0, Math.min(h - 1, Math.round((y - c.top) / c.s))),
    };
  };
  const toClient = (x: number, y: number) => {
    const c = content();
    return { x: c.left + x * c.s, y: c.top + y * c.s };
  };

  const applyTransform = () => {
    const c = canvas.current, s = stage.current;
    if (!c || !s) return;
    const W = s.clientWidth, H = s.clientHeight;
    st.z = Math.max(1, Math.min(6, st.z));
    st.tx = Math.min(0, Math.max(W - W * st.z, st.tx));
    st.ty = Math.min(0, Math.max(H - H * st.z, st.ty));
    c.style.transformOrigin = '0 0';
    c.style.transform = st.z === 1 ? '' : `translate(${st.tx}px, ${st.ty}px) scale(${st.z})`;
    setZoomed(st.z > 1.01);
  };
  const resetZoom = () => { st.z = 1; st.tx = 0; st.ty = 0; applyTransform(); };

  // keep the trackpad cursor visible while zoomed
  const followCursor = () => {
    const s = stage.current;
    if (!s || st.z <= 1) return;
    const r = s.getBoundingClientRect();
    const p = toClient(st.cx, st.cy);
    const m = 48;
    let dx = 0, dy = 0;
    if (p.x < r.left + m) dx = r.left + m - p.x;
    if (p.x > r.right - m) dx = r.right - m - p.x;
    if (p.y < r.top + m) dy = r.top + m - p.y;
    if (p.y > r.bottom - m) dy = r.bottom - m - p.y;
    if (dx || dy) { st.tx += dx; st.ty += dy; applyTransform(); }
  };

  // ---------- sending ----------
  const flushMove = () => {
    if (st.pending) return;
    st.pending = true;
    requestAnimationFrame(() => { st.pending = false; send({ t: 'mm', x: st.cx, y: st.cy }); });
  };
  const moveTo = (x: number, y: number) => { st.cx = x; st.cy = y; flushMove(); };
  const click = (b: number) => {
    send({ t: 'mm', x: st.cx, y: st.cy });
    send({ t: 'md', b, x: st.cx, y: st.cy });
    send({ t: 'mu', b, x: st.cx, y: st.cy });
  };
  const down = (b: number) => send({ t: 'md', b, x: st.cx, y: st.cy });
  const up = (b: number) => send({ t: 'mu', b, x: st.cx, y: st.cy });

  const pressKey = (code: string, extra: string[] = []) => {
    const m = [...new Set([...modsRef.current, ...extra])];
    m.forEach((c) => send({ t: 'kd', code: c }));
    send({ t: 'kd', code });
    send({ t: 'ku', code });
    m.slice().reverse().forEach((c) => send({ t: 'ku', code: c }));
    if (modsRef.current.length) setMods([]);
  };
  const typeText = (text: string) => {
    if (!text) return;
    const m = modsRef.current;
    if (m.length && text.length === 1) {
      const code = charCode(text);
      if (code) { pressKey(code); return; }
    }
    const parts = text.split('\n');
    parts.forEach((p, i) => {
      if (p) send({ t: 'type', text: p });
      if (i < parts.length - 1) pressKey('Enter');
    });
    if (m.length) setMods([]);
  };

  // ---------- gestures ----------
  useEffect(() => {
    const el = stage.current;
    if (!el) return;
    type G = {
      kind: 'none' | 'one' | 'two';
      sx: number; sy: number; lx: number; ly: number; t0: number;
      moved: boolean; dragging: boolean; longDone: boolean; timer: any;
      multi: boolean; twoKind: 'undecided' | 'pinch' | 'scroll';
      d0: number; mx0: number; my0: number; z0: number; tx0: number; ty0: number;
      sax: number; say: number; lastTap: number; tapDrag: boolean;
    };
    const g: G = {
      kind: 'none', sx: 0, sy: 0, lx: 0, ly: 0, t0: 0, moved: false, dragging: false, longDone: false, timer: 0,
      multi: false, twoKind: 'undecided', d0: 0, mx0: 0, my0: 0, z0: 1, tx0: 0, ty0: 0, sax: 0, say: 0, lastTap: 0, tapDrag: false,
    };
    const clearTimer = () => { if (g.timer) { clearTimeout(g.timer); g.timer = 0; } };
    const two = (t: TouchList) => {
      const a = t[0], b = t[1];
      return { d: Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY), mx: (a.clientX + b.clientX) / 2, my: (a.clientY + b.clientY) / 2 };
    };
    const endDrag = () => { if (g.dragging) { up(0); g.dragging = false; } };

    const start = (e: TouchEvent) => {
      e.preventDefault();
      const t = e.targetTouches;
      if (t.length === 1 && g.kind === 'none') {
        const p = t[0];
        const now = Date.now();
        Object.assign(g, { kind: 'one', sx: p.clientX, sy: p.clientY, lx: p.clientX, ly: p.clientY, t0: now, moved: false, longDone: false, multi: false });
        g.tapDrag = modeRef.current === 'trackpad' && now - g.lastTap < 300;
        if (g.tapDrag) { down(0); g.dragging = true; }
        clearTimer();
        g.timer = setTimeout(() => {
          g.timer = 0;
          if (g.moved || g.kind !== 'one' || g.dragging) return;
          g.longDone = true;
          navigator.vibrate?.(15);
          if (modeRef.current === 'touch') {
            const r = toRemote(g.sx, g.sy);
            moveTo(r.x, r.y);
            click(2);
          } else {
            down(0); g.dragging = true; // long press = start a drag in trackpad mode
          }
        }, LONG_MS);
      } else if (t.length === 2) {
        clearTimer();
        if (g.dragging && modeRef.current === 'touch') endDrag();
        const q = two(t);
        Object.assign(g, { kind: 'two', multi: true, twoKind: 'undecided', d0: q.d, mx0: q.mx, my0: q.my, z0: st.z, tx0: st.tx, ty0: st.ty, sax: 0, say: 0, t0: Date.now(), lx: q.mx, ly: q.my });
      }
    };

    const move = (e: TouchEvent) => {
      e.preventDefault();
      const t = e.targetTouches;
      if (g.kind === 'one' && t.length === 1) {
        const p = t[0];
        const dx = p.clientX - g.lx, dy = p.clientY - g.ly;
        g.lx = p.clientX; g.ly = p.clientY;
        if (!g.moved && Math.hypot(p.clientX - g.sx, p.clientY - g.sy) > TAP_SLOP) { g.moved = true; clearTimer(); }
        if (!g.moved) return;
        if (modeRef.current === 'trackpad') {
          const c = content();
          const speed = Math.hypot(dx, dy);
          const accel = 1 + Math.min(speed / 12, 1.5); // faster swipes travel further
          const { w, h } = remote();
          st.cx = Math.max(0, Math.min(w - 1, st.cx + (dx * accel) / c.s));
          st.cy = Math.max(0, Math.min(h - 1, st.cy + (dy * accel) / c.s));
          st.cx = Math.round(st.cx); st.cy = Math.round(st.cy);
          flushMove();
          followCursor();
        } else {
          if (!g.dragging && !g.longDone) {
            const r0 = toRemote(g.sx, g.sy);
            st.cx = r0.x; st.cy = r0.y;
            send({ t: 'mm', x: r0.x, y: r0.y });
            down(0);
            g.dragging = true;
          }
          const r = toRemote(p.clientX, p.clientY);
          moveTo(r.x, r.y);
        }
      } else if (g.kind === 'two' && t.length >= 2) {
        const q = two(t);
        if (g.twoKind === 'undecided') {
          if (Math.abs(q.d - g.d0) > 28) g.twoKind = 'pinch';
          else if (Math.hypot(q.mx - g.mx0, q.my - g.my0) > 12) g.twoKind = 'scroll';
        }
        if (g.twoKind === 'pinch') {
          const r = stage.current!.getBoundingClientRect();
          const z = Math.max(1, Math.min(6, g.z0 * (q.d / g.d0)));
          // keep the point that was under the fingers under the fingers
          const px = (g.mx0 - r.left - g.tx0) / g.z0, py = (g.my0 - r.top - g.ty0) / g.z0;
          st.z = z;
          st.tx = q.mx - r.left - px * z;
          st.ty = q.my - r.top - py * z;
          applyTransform();
        } else if (g.twoKind === 'scroll') {
          g.sax += q.mx - g.lx; g.say += q.my - g.ly;
          g.lx = q.mx; g.ly = q.my;
          const step = 36;
          const ny = Math.trunc(g.say / step), nx = Math.trunc(g.sax / step);
          if (ny || nx) {
            g.say -= ny * step; g.sax -= nx * step;
            // natural scrolling: fingers up = content moves up (scroll down)
            send({ t: 'wh', dx: -nx, dy: -ny });
          }
        }
      }
    };

    const end = (e: TouchEvent) => {
      e.preventDefault();
      const left = e.targetTouches.length;
      const quick = Date.now() - g.t0 < TAP_MS;
      if (g.kind === 'two') {
        if (left === 0) {
          if (g.twoKind === 'undecided' && quick) {
            if (modeRef.current === 'touch') { const r = toRemote(g.mx0, g.my0); moveTo(r.x, r.y); }
            click(2);
          }
          g.kind = 'none';
        }
        return;
      }
      if (g.kind === 'one' && left === 0) {
        clearTimer();
        if (!g.moved && !g.longDone && quick) {
          if (g.tapDrag) {
            endDrag();
            click(0); // double tap = double click
          } else {
            if (modeRef.current === 'touch') {
              const r = toRemote(g.sx, g.sy);
              moveTo(r.x, r.y);
            }
            click(0);
          }
          g.lastTap = g.tapDrag ? 0 : Date.now();
        } else {
          g.lastTap = 0;
        }
        endDrag();
        g.kind = 'none';
      }
    };

    el.addEventListener('touchstart', start, { passive: false });
    el.addEventListener('touchmove', move, { passive: false });
    el.addEventListener('touchend', end, { passive: false });
    el.addEventListener('touchcancel', end, { passive: false });
    return () => {
      clearTimer();
      el.removeEventListener('touchstart', start);
      el.removeEventListener('touchmove', move);
      el.removeEventListener('touchend', end);
      el.removeEventListener('touchcancel', end);
    };
  }, []);

  // cursor overlay follows the remote cursor (trackpad mode)
  useEffect(() => {
    let raf = 0;
    const loop = () => {
      raf = requestAnimationFrame(loop);
      const el = cursorEl.current;
      const c = canvas.current;
      if (!c) return;
      // first frame, display switch or quality change: keep the cursor at the same relative spot
      if (c.width !== st.lw || c.height !== st.lh) {
        if (st.cx < 0 || !st.lw) { st.cx = Math.round(c.width / 2); st.cy = Math.round(c.height / 2); }
        else { st.cx = Math.round((st.cx * c.width) / st.lw); st.cy = Math.round((st.cy * c.height) / st.lh); }
        st.lw = c.width; st.lh = c.height;
      }
      if (!el) return;
      const p = toClient(st.cx, st.cy);
      el.style.transform = `translate(${p.x}px, ${p.y}px)`;
    };
    loop();
    const onResize = () => applyTransform();
    window.addEventListener('resize', onResize);
    return () => { cancelAnimationFrame(raf); window.removeEventListener('resize', onResize); };
  }, []);

  // keep the bar above the on-screen keyboard (iOS/Android shrink the visual viewport)
  useEffect(() => {
    const vv = window.visualViewport;
    const root = stage.current?.closest('.rd') as HTMLElement | null;
    if (!vv || !root) return;
    const fit = () => {
      root.style.height = vv.height + 'px';
      root.style.transform = vv.offsetTop ? `translateY(${vv.offsetTop}px)` : '';
    };
    fit();
    vv.addEventListener('resize', fit);
    vv.addEventListener('scroll', fit);
    return () => { vv.removeEventListener('resize', fit); vv.removeEventListener('scroll', fit); root.style.height = ''; root.style.transform = ''; };
  }, []);

  // ---------- keyboard ----------
  const last = useRef(SENTINEL);
  const composing = useRef(false);
  const resetBox = () => {
    const ta = kbd.current;
    if (!ta) return;
    ta.value = SENTINEL;
    last.current = SENTINEL;
    ta.setSelectionRange(SENTINEL.length, SENTINEL.length);
  };
  const onInput = () => {
    const ta = kbd.current!;
    const v = ta.value, prev = last.current;
    let p = 0;
    while (p < v.length && p < prev.length && v[p] === prev[p]) p++;
    const del = prev.length - p;
    for (let i = 0; i < del; i++) pressKey('Backspace');
    typeText(v.slice(p));
    last.current = v;
    if (!composing.current && v !== SENTINEL) resetBox();
  };
  useEffect(() => {
    const ta = kbd.current;
    if (!ta) return;
    const cs = () => { composing.current = true; };
    const ce = () => { composing.current = false; onInput(); };
    ta.addEventListener('compositionstart', cs);
    ta.addEventListener('compositionend', ce);
    return () => { ta.removeEventListener('compositionstart', cs); ta.removeEventListener('compositionend', ce); };
  }, []);
  const onKeyDown = (e: KeyboardEvent) => {
    if (e.isComposing || e.keyCode === 229) return;
    const extra: string[] = [];
    if (e.ctrlKey) extra.push('ControlLeft');
    if (e.altKey) extra.push('AltLeft');
    if (e.metaKey) extra.push('MetaLeft');
    if (SPECIAL.has(e.code) || SPECIAL.has(e.key) || (extra.length && e.code)) {
      e.preventDefault();
      if (e.shiftKey && extra.length) extra.push('ShiftLeft');
      pressKey(SPECIAL.has(e.code) ? e.code : SPECIAL.has(e.key) ? e.key : e.code, extra);
    }
  };
  const toggleKeyboard = () => {
    const ta = kbd.current!;
    if (kbOpen) { ta.blur(); setKbOpen(false); return; }
    resetBox();
    ta.focus();
    setKbOpen(true);
  };
  const toggleMod = (code: string) => setMods((m) => (m.includes(code) ? m.filter((c) => c !== code) : [...m, code]));

  // hold-to-drag mouse buttons (press and hold Left, then move with another finger)
  const btn = (b: number) => ({
    onTouchStart: (e: TouchEvent) => { e.preventDefault(); down(b); setHeld(b); navigator.vibrate?.(8); },
    onTouchEnd: (e: TouchEvent) => { e.preventDefault(); up(b); setHeld(null); },
    onTouchCancel: () => { up(b); setHeld(null); },
    onClick: () => click(b), // mouse / accessibility fallback
  });

  // tapping a button must not close the phone keyboard
  const keepFocus = (e: MouseEvent) => { if ((e.target as HTMLElement).closest('button')) e.preventDefault(); };

  const changeMode = (m: TouchMode) => { setMode(m); saveTouchMode(m); };

  return (
    <>
      {mode === 'trackpad' && <div ref={cursorEl} class="rd-cursor" aria-hidden="true"><svg viewBox="0 0 24 24" width="22" height="22"><path d="M4 2l15 9-6.5 1.6L9.4 19z" /></svg></div>}
      <textarea
        ref={kbd}
        class="rd-kbd-input"
        autocapitalize="off"
        autocomplete="off"
        autocorrect="off"
        spellcheck={false}
        aria-label="Keyboard input for the remote device"
        onInput={onInput}
        onKeyDown={onKeyDown}
        onBlur={() => setKbOpen(false)}
      />
      {keysOpen && (
        <div class="rd-keys" onMouseDown={keepFocus}>
          <div class="rd-keys-row">
            {MODS.map((m) => (
              <button key={m.code} class={'rd-key mod' + (mods.includes(m.code) ? ' on' : '')} onClick={() => toggleMod(m.code)}>{m.label}</button>
            ))}
            <button class="rd-key" onClick={onCad}>Ctrl+Alt+Del</button>
          </div>
          <div class="rd-keys-row">
            {KEYS.map((k) => <button key={k.code} class="rd-key" onClick={() => pressKey(k.code)}>{k.label}</button>)}
          </div>
        </div>
      )}
      <div class="rd-touchbar" onMouseDown={keepFocus}>
        <div class="rd-seg" role="group" aria-label="Touch mode">
          <button class={mode === 'trackpad' ? 'on' : ''} onClick={() => changeMode('trackpad')} aria-label="Trackpad" title="Trackpad mode"><Icon name="pointer" size={15} /><span class="rd-seg-label">Trackpad</span></button>
          <button class={mode === 'touch' ? 'on' : ''} onClick={() => changeMode('touch')} aria-label="Touch" title="Touch mode"><span class="rd-finger" /><span class="rd-seg-label">Touch</span></button>
        </div>
        <button class={'rd-mbtn' + (held === 0 ? ' on' : '')} {...btn(0)}>Left</button>
        <button class={'rd-mbtn' + (held === 2 ? ' on' : '')} {...btn(2)}>Right</button>
        <button class={'rd-tbtn' + (kbOpen ? ' on' : '')} onClick={toggleKeyboard} aria-label="Keyboard"><Icon name="keyboard" size={18} /></button>
        <button class={'rd-tbtn' + (keysOpen ? ' on' : '')} onClick={() => setKeysOpen(!keysOpen)} aria-label="Special keys">
          <span class="rd-fn">{mods.length ? MODS.filter((m) => mods.includes(m.code)).map((m) => m.label).join('+') : 'Fn'}</span>
        </button>
        {zoomed && <button class="rd-tbtn" onClick={resetZoom} aria-label="Reset zoom">1×</button>}
      </div>
    </>
  );
}

export function TouchHelp({ mode, onClose }: { mode: TouchMode; onClose: () => void }) {
  const rows = mode === 'trackpad'
    ? [['Move the cursor', 'Drag one finger'], ['Left click', 'Tap'], ['Right click', 'Two-finger tap'], ['Scroll', 'Two-finger drag'], ['Drag', 'Double-tap and drag, or long press then drag'], ['Zoom', 'Pinch']]
    : [['Click', 'Tap where you want to click'], ['Right click', 'Long press or two-finger tap'], ['Drag', 'Touch and drag'], ['Scroll', 'Two-finger drag'], ['Zoom', 'Pinch']];
  return (
    <div class="rd-help" onClick={onClose}>
      <div class="rd-help-card" onClick={(e) => e.stopPropagation()}>
        <strong>{mode === 'trackpad' ? 'Trackpad mode' : 'Touch mode'}</strong>
        <table>{rows.map(([a, b]) => <tr key={a}><td>{a}</td><td>{b}</td></tr>)}</table>
        <p>Left and Right on the bar click at the cursor. Hold Left and drag with another finger to drag things. The keyboard button opens your phone keyboard; Fn has Esc, Tab, arrows, F-keys and sticky Ctrl / Alt / Shift / Win.</p>
        <button class="rd-key" onClick={onClose}>Got it</button>
      </div>
    </div>
  );
}
