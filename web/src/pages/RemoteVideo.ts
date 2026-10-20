// H.264 video for the remote viewer: the agent sends Annex B access units, decoded here with
// WebCodecs (hardware accelerated where the browser can) and painted onto the canvas.
//
// Video message: 0x03 | u8 flags (1 = keyframe) | u32 seq | H.264 Annex B access unit

export type StreamSettings = {
  mode: 'auto' | 'images'; // auto = video when this browser and the device can do it
  fps: number; // 15 / 30 / 60 / 120
  bw: number; // kbit/s limit, 0 = automatic
  scale: number; // 1, 0.75, 0.5
  direct: boolean; // try WebRTC for a direct path
  clip: boolean; // share the clipboard with the remote computer
};

export const DEFAULT_SETTINGS: StreamSettings = { mode: 'auto', fps: 60, bw: 0, scale: 1, direct: true, clip: true };

export function loadSettings(): StreamSettings {
  try {
    const s = JSON.parse(localStorage.getItem('apex-rd-stream') || '{}');
    return { ...DEFAULT_SETTINGS, ...s };
  } catch {
    return { ...DEFAULT_SETTINGS };
  }
}

export function saveSettings(s: StreamSettings) {
  try { localStorage.setItem('apex-rd-stream', JSON.stringify(s)); } catch {}
}

/** The "opt" message for the agent. vc = this viewer can decode H.264. */
export function optMessage(s: StreamSettings, vc: boolean) {
  const q = s.bw === 0 ? 65 : s.bw < 3000 ? 40 : s.bw < 10000 ? 60 : 80; // JPEG quality for image mode
  return { t: 'opt', q, s: s.scale, fps: s.fps, v: 2, vc, bw: s.bw, mode: s.mode, clip: s.clip };
}

let support: Promise<boolean> | null = null;
/** Whether this browser can decode H.264 with WebCodecs. */
export function h264Supported(): Promise<boolean> {
  if (!support) {
    support = (async () => {
      const VD = (window as any).VideoDecoder;
      if (!VD) return false;
      try {
        const r = await VD.isConfigSupported({ codec: 'avc1.42E033', optimizeForLatency: true });
        return !!r.supported;
      } catch {
        return false;
      }
    })();
  }
  return support;
}

// codecString reads profile/constraints/level from the first SPS in an Annex B buffer.
function codecString(b: Uint8Array): string | null {
  for (let i = 0; i + 4 < b.length; i++) {
    if (b[i] === 0 && b[i + 1] === 0 && b[i + 2] === 1 && (b[i + 3] & 0x1f) === 7 && i + 6 < b.length) {
      const h = (x: number) => x.toString(16).padStart(2, '0');
      return 'avc1.' + h(b[i + 4]) + h(b[i + 5]) + h(b[i + 6]);
    }
  }
  return null;
}

export class VideoSink {
  private dec: any = null;
  private configured = '';
  private waitingKey = true;
  frames = 0; // frames painted (reset by the caller for fps)
  constructor(
    private canvas: () => HTMLCanvasElement | null,
    private onPainted: (seq: number) => void,
    private requestKey: () => void,
  ) {}

  /** A new stream (size change or switch to video): wait for the next keyframe. */
  reset() {
    this.waitingKey = true;
    try { this.dec?.close(); } catch {}
    this.dec = null;
    this.configured = '';
  }

  private ensure(codec: string) {
    if (this.dec && this.dec.state === 'configured' && this.configured === codec) return;
    try { this.dec?.close(); } catch {}
    const VD = (window as any).VideoDecoder;
    this.dec = new VD({
      output: (frame: any) => {
        const c = this.canvas();
        const ctx = c?.getContext('2d');
        if (ctx) ctx.drawImage(frame, 0, 0, c!.width, c!.height);
        const seq = Math.round(frame.timestamp / 1000);
        frame.close();
        this.frames++;
        this.onPainted(seq);
      },
      error: () => {
        // Corrupt or unsupported data: start over from a keyframe.
        this.reset();
        this.requestKey();
      },
    });
    this.dec.configure({ codec, optimizeForLatency: true, hardwareAcceleration: 'no-preference' });
    this.configured = codec;
  }

  push(buf: ArrayBuffer) {
    const v = new DataView(buf);
    const key = (v.getUint8(1) & 1) === 1;
    const seq = v.getUint32(2);
    const data = new Uint8Array(buf, 6);
    if (this.waitingKey) {
      if (!key) {
        this.onPainted(seq); // keep the agent's flow control moving
        return;
      }
      this.waitingKey = false;
    }
    if (key) {
      const codec = codecString(data);
      if (codec) this.ensure(codec);
    }
    if (!this.dec || this.dec.state !== 'configured') return;
    if (this.dec.decodeQueueSize > 30) { // hopelessly behind: resync
      this.reset();
      this.requestKey();
      return;
    }
    const Chunk = (window as any).EncodedVideoChunk;
    this.dec.decode(new Chunk({ type: key ? 'key' : 'delta', timestamp: seq * 1000, data }));
  }

  close() {
    try { this.dec?.close(); } catch {}
    this.dec = null;
  }
}
