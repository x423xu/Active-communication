package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
)

type SignalMsg struct {
	Type      string                   `json:"type"`
	Offer     *SessionDescWrap         `json:"offer,omitempty"`
	Answer    *SessionDescWrap         `json:"answer,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
}

type SessionDescWrap struct {
	Type string `json:"type"`
	Sdp  string `json:"sdp"`
}

var up = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func main() {
	http.HandleFunc("/ws", wsHandler)
	log.Println("[HTTP] ws on :8765/ws")
	log.Fatal(http.ListenAndServe(":8765", nil))
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	defer ws.Close()
	log.Println("[WS] client connected")

	cfg := webrtc.Configuration{
		ICETransportPolicy: webrtc.ICETransportPolicyAll, // keep simple while debugging
	}

	pc, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		log.Println("NewPeerConnection:", err)
		return
	}
	defer pc.Close()

	// --- GS DataChannel (client-created) ---
	var gsOpen atomic.Bool
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Println("[DC] received:", dc.Label())
		if dc.Label() != "gs" {
			return
		}
		dc.OnOpen(func() {
			gsOpen.Store(true)
			log.Println("[DC] gs open")
			go func() {
				t := time.NewTicker(1 * time.Second)
				defer t.Stop()
				for range t.C {
					if !gsOpen.Load() {
						return
					}
					_ = dc.SendText(`{"type":"heartbeat","from":"webrtc"}`)
				}
			}()
		})
		dc.OnClose(func() {
			gsOpen.Store(false)
			log.Println("[DC] gs close")
		})
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Println("[RTC] ICE:", s.String())
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Println("[RTC] PC:", s.String())
	})

	// --- Outgoing echo track: send proper VP8 frames as Samples ---
	echoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video", "echo",
	)
	if err != nil {
		log.Println("NewTrackLocalStaticSample:", err)
		return
	}
	_, err = pc.AddTrack(echoTrack)
	if err != nil {
		log.Println("AddTrack:", err)
		return
	}

	// ICE candidates back to browser
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = ws.WriteJSON(SignalMsg{Type: "candidate", Candidate: ptr(c.ToJSON())})
	})

	// Handle incoming media
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if tr.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		log.Println("[RTC] OnTrack video codec:", tr.Codec().MimeType)

		// Request keyframes aggressively at the start, then periodically
		go func(ssrc uint32) {
			for i := 0; i < 8; i++ {
				_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
				time.Sleep(150 * time.Millisecond)
			}
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for range t.C {
				_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
			}
		}(uint32(tr.SSRC()))

		// ✅ Correct: build frames from full RTP packets
		depacketizer := &codecs.VP8Packet{}
		sb := samplebuilder.New(100, depacketizer, tr.Codec().ClockRate)

		last := time.Now()
		var wrote int64

		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				log.Println("[RTC] Track read ended:", err)
				return
			}
			sb.Push(pkt)

			for {
				sample := sb.Pop()
				if sample == nil {
					break
				}

				// Estimate duration from wall clock (keeps playback smooth)
				now := time.Now()
				dur := now.Sub(last)
				if dur <= 0 || dur > 200*time.Millisecond {
					dur = 33 * time.Millisecond
				}
				last = now

				if err := echoTrack.WriteSample(media.Sample{Data: sample.Data, Duration: dur}); err == nil {
					wrote++
					if wrote%60 == 0 {
						log.Println("[ECHO] wrote samples:", wrote)
					}
				}
			}
		}
	})

	// --- Signaling loop ---
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			log.Println("[WS] read:", err)
			return
		}
		var msg SignalMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Println("[WS] json unmarshal error:", err)
			continue
		}

		switch msg.Type {
		case "offer":
			if msg.Offer == nil {
				log.Println("[WS] offer missing")
				continue
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  msg.Offer.Sdp,
			}); err != nil {
				log.Println("SetRemoteDescription:", err)
				continue
			}

			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Println("CreateAnswer:", err)
				continue
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				log.Println("SetLocalDescription:", err)
				continue
			}

			if err := ws.WriteJSON(SignalMsg{
				Type: "answer",
				Answer: &SessionDescWrap{
					Type: "answer",
					Sdp:  answer.SDP,
				},
			}); err != nil {
				log.Println("[WS] WriteJSON answer error:", err)
				continue
			}
			log.Println("[RTC] sent answer")

		case "candidate":
			if msg.Candidate == nil {
				continue
			}
			if err := pc.AddICECandidate(*msg.Candidate); err != nil {
				log.Println("AddICECandidate:", err)
			}
		default:
			log.Println("[WS] unknown msg:", fmt.Sprintf("%+v", msg))
		}
	}
}

func ptr[T any](v T) *T { return &v }
