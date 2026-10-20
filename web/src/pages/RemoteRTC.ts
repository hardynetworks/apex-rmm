// WebRTC direct path for the remote viewer (see internal/agent/desktop/rtc.go). The viewer
// offers to receive one H.264 track and opens an "input" data channel; signalling uses the
// session websocket. When the agent says "rtc-on", frames arrive over RTP instead of the
// websocket and are painted from a hidden <video> element onto the same canvas.

const ICE_SERVERS = [{ urls: ['stun:stun.l.google.com:19302', 'stun:stun.cloudflare.com:3478'] }];

export class RTCLink {
  private pc: RTCPeerConnection | null = null;
  private dc: RTCDataChannel | null = null;
  private video: HTMLVideoElement;
  private live = false;
  private drawing = 0;
  frames = 0;

  constructor(
    private sendSignal: (m: any) => void,
    private canvas: () => HTMLCanvasElement | null,
    private onPainted: () => void,
    private onState: (s: 'connecting' | 'direct' | 'relay', why?: string) => void,
  ) {
    this.video = document.createElement('video');
    this.video.muted = true;
    this.video.playsInline = true;
    this.video.autoplay = true;
    this.video.setAttribute('aria-hidden', 'true');
    this.video.style.cssText = 'position:fixed;left:0;top:0;width:2px;height:2px;opacity:0;pointer-events:none';
    document.body.appendChild(this.video);
  }

  static supported() {
    return typeof RTCPeerConnection !== 'undefined';
  }

  async start() {
    this.stop();
    this.onState('connecting');
    const pc = new RTCPeerConnection({ iceServers: ICE_SERVERS, bundlePolicy: 'max-bundle' });
    this.pc = pc;
    const tr = pc.addTransceiver('video', { direction: 'recvonly' });
    try {
      const caps = (RTCRtpReceiver as any).getCapabilities?.('video');
      const h264 = caps?.codecs?.filter((c: any) => /h264/i.test(c.mimeType) && /packetization-mode=1/.test(c.sdpFmtpLine || '')) || [];
      if (h264.length && tr.setCodecPreferences) tr.setCodecPreferences(h264);
    } catch {}
    this.dc = pc.createDataChannel('input', { ordered: true });
    pc.ontrack = (ev) => {
      try { (ev.receiver as any).jitterBufferTarget = 0; } catch {}
      try { (ev.receiver as any).playoutDelayHint = 0; } catch {}
      this.video.srcObject = ev.streams[0] || new MediaStream([ev.track]);
      this.video.play().catch(() => {});
    };
    pc.onconnectionstatechange = () => {
      if (pc !== this.pc) return;
      if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
        this.setLive(false);
        this.onState('relay', 'no direct connection');
      }
    };
    await pc.setLocalDescription(await pc.createOffer());
    // Send the offer with whatever candidates we have after a short wait (no trickle).
    await new Promise<void>((resolve) => {
      if (pc.iceGatheringState === 'complete') return resolve();
      const t = setTimeout(resolve, 2500);
      pc.onicegatheringstatechange = () => { if (pc.iceGatheringState === 'complete') { clearTimeout(t); resolve(); } };
    });
    if (pc !== this.pc) return;
    this.sendSignal({ t: 'rtc-offer', sdp: pc.localDescription!.sdp });
    // Give up on the direct path if it isn't carrying video within a while.
    setTimeout(() => { if (pc === this.pc && !this.live) this.onState('relay', 'no direct connection'); }, 12000);
  }

  /** Handles rtc-* messages from the agent. Returns true if it was one. */
  handle(m: any): boolean {
    switch (m.t) {
      case 'rtc-answer':
        this.pc?.setRemoteDescription({ type: 'answer', sdp: m.sdp }).catch(() => this.onState('relay', 'bad answer'));
        return true;
      case 'rtc-on':
        this.setLive(true);
        return true;
      case 'rtc-off':
        this.setLive(false);
        this.onState('relay', m.error || 'direct connection lost');
        return true;
    }
    return false;
  }

  private setLive(on: boolean) {
    if (on === this.live) return;
    this.live = on;
    if (on) {
      this.onState('direct');
      this.paintLoop();
    } else if (this.drawing) {
      (this.video as any).cancelVideoFrameCallback?.(this.drawing);
      this.drawing = 0;
    }
  }

  private paintLoop() {
    const v = this.video as any;
    const paint = () => {
      if (!this.live) return;
      const c = this.canvas();
      const ctx = c?.getContext('2d');
      if (ctx && v.videoWidth) {
        ctx.drawImage(this.video, 0, 0, c!.width, c!.height);
        this.frames++;
        this.onPainted();
      }
      this.drawing = v.requestVideoFrameCallback ? v.requestVideoFrameCallback(paint) : requestAnimationFrame(paint);
    };
    this.drawing = v.requestVideoFrameCallback ? v.requestVideoFrameCallback(paint) : requestAnimationFrame(paint);
  }

  /** Sends input over the data channel when the direct path is up. Returns false otherwise. */
  sendInput(m: any): boolean {
    if (!this.live || !this.dc || this.dc.readyState !== 'open') return false;
    this.dc.send(JSON.stringify(m));
    return true;
  }

  get isLive() { return this.live; }

  stop() {
    if (this.pc) this.sendSignal({ t: 'rtc-stop' });
    this.setLive(false);
    try { this.dc?.close(); } catch {}
    try { this.pc?.close(); } catch {}
    this.pc = null;
    this.dc = null;
  }

  destroy() {
    this.stop();
    this.video.srcObject = null;
    this.video.remove();
  }
}
