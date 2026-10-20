package desktop

import (
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// WebRTC gives the session a direct path between the viewer and the device when one exists
// (same LAN, both on NetBird/a VPN, or NAT traversal via STUN). Signalling goes over the
// session websocket; once the peer connection is up the H.264 frames go over RTP (with GCC
// congestion control) and input over a data channel, so neither has to pass through the
// server. If it never connects, or drops, the session carries on over the websocket.
//
//	viewer → agent  {"t":"rtc-offer","sdp":"…"}   (offer with all candidates)
//	agent → viewer  {"t":"rtc-answer","sdp":"…"} | {"t":"rtc-off","error":"…"}
//	agent → viewer  {"t":"rtc-on"} when video starts flowing over WebRTC, {"t":"rtc-off"} when it stops
type rtcPeer struct {
	pc        *webrtc.PeerConnection
	track     *webrtc.TrackLocalStaticSample
	estimator cc.BandwidthEstimator
	mu        sync.Mutex
	up        bool
	lastFrame time.Time
}

var stunServers = []string{"stun:stun.l.google.com:19302", "stun:stun.cloudflare.com:3478"}

// newRTCPeer answers the viewer's offer. onInput receives data-channel messages (same JSON as
// on the websocket), onKey is called for keyframe requests (PLI/FIR), onState when the
// connection comes up (true) or goes away (false).
func newRTCPeer(offer string, onInput func([]byte), onKey func(), onState func(bool)) (*rtcPeer, string, error) {
	m := &webrtc.MediaEngine{}
	// Constrained baseline, packetization mode 1: what the agent encodes and every browser decodes.
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}, {Type: "transport-cc"}},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, "", err
	}
	reg := &interceptor.Registry{}
	ccI, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		// No packet pacer: the session paces whole frames to the estimate itself, and a
		// per-packet pacer holds big keyframes back for hundreds of milliseconds.
		return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(3_000_000), gcc.SendSideBWEMinBitrate(minBitrate),
			gcc.SendSideBWEMaxBitrate(autoMaxRate), gcc.SendSideBWEPacer(gcc.NewNoOpPacer()))
	})
	if err != nil {
		return nil, "", err
	}
	estimators := make(chan cc.BandwidthEstimator, 1)
	ccI.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) { estimators <- e })
	reg.Add(ccI)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, reg); err != nil {
		return nil, "", err
	}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, "", err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: stunServers}}})
	if err != nil {
		return nil, "", err
	}
	p := &rtcPeer{pc: pc}
	select {
	case p.estimator = <-estimators:
	case <-time.After(5 * time.Second):
		pc.Close()
		return nil, "", errors.New("bandwidth estimator didn't start")
	}
	p.track, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "screen", "apex")
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	sender, err := pc.AddTrack(p.track)
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	go func() { // keyframe requests from the viewer
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, pkt := range pkts {
				switch pkt.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					onKey()
				}
			}
		}
	}()
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { onInput(msg.Data) })
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			p.mu.Lock()
			p.up = true
			p.mu.Unlock()
			onState(true)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			p.mu.Lock()
			was := p.up
			p.up = false
			p.mu.Unlock()
			if was {
				onState(false)
			}
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		pc.Close()
		return nil, "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return nil, "", err
	}
	select { // STUN can be slow or blocked; host candidates are there right away
	case <-gathered:
	case <-time.After(3 * time.Second):
	}
	return p, pc.LocalDescription().SDP, nil
}

// WriteFrame sends one access unit (Annex B) over RTP.
func (p *rtcPeer) WriteFrame(au []byte) error {
	now := time.Now()
	d := 16 * time.Millisecond
	if !p.lastFrame.IsZero() {
		d = now.Sub(p.lastFrame)
	}
	p.lastFrame = now
	return p.track.WriteSample(media.Sample{Data: au, Duration: d})
}

// Bitrate is the congestion controller's current estimate (bit/s).
func (p *rtcPeer) Bitrate() int { return p.estimator.GetTargetBitrate() }

func (p *rtcPeer) Up() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.up
}

func (p *rtcPeer) Close() {
	if err := p.pc.Close(); err != nil {
		log.Printf("webrtc close: %v", err)
	}
}

// rtcSignal is the JSON of the signalling messages.
type rtcSignal struct {
	T   string `json:"t"`
	SDP string `json:"sdp"`
}

func parseRTC(b []byte) (rtcSignal, bool) {
	var s rtcSignal
	if json.Unmarshal(b, &s) != nil || s.T != "rtc-offer" || s.SDP == "" {
		return s, false
	}
	return s, true
}
