package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type Msg struct {
	Type      string                    `json:"type"`
	Offer     webrtc.SessionDescription `json:"offer"`
	Answer    webrtc.SessionDescription `json:"answer"`
	Candidate webrtc.ICECandidateInit   `json:"candidate"`
}

var up = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func main() {
	turnHost := os.Getenv("TURN_HOST")
	if turnHost == "" {
		log.Fatal("TURN_HOST env var required")
	}
	turnUser := getenv("TURN_USER", "test")
	turnPass := getenv("TURN_PASS", "test123")

	cfg := webrtc.Configuration{
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
		ICEServers: []webrtc.ICEServer{{
			URLs:       []string{"turn:" + turnHost + ":3478?transport=tcp"},
			Username:   turnUser,
			Credential: turnPass,
		}},
	}

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			log.Println("upgrade:", err)
			return
		}
		defer ws.Close()
		log.Println("[WS] client connected")

		pc, err := webrtc.NewPeerConnection(cfg)
		if err != nil {
			log.Println("pc:", err)
			return
		}
		defer pc.Close()

		pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
			log.Println("[RTC] iceConnectionState:", s.String())
		})
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			log.Println("[RTC] connectionState:", s.String())
		})

		// --- PRE-ADD outgoing echo track so it appears in SDP answer ---
		// Use VP8 (Chrome default) for maximum compatibility.
		echoTrack, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
			"video", "echo",
		)
		if err != nil {
			log.Println("NewTrackLocalStaticRTP:", err)
			return
		}
		echoSender, err := pc.AddTrack(echoTrack)
		if err != nil {
			log.Println("AddTrack:", err)
			return
		}

		// Pion needs us to read RTCP for the sender to avoid blocking
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if _, _, err := echoSender.Read(rtcpBuf); err != nil {
					return
				}
			}
		}()

		// We'll rewrite payload type to match what was negotiated for the sender.
		var negotiatedPT atomic.Uint32 // stores payload type as uint32
		negotiatedPT.Store(0)

		pc.OnICECandidate(func(c *webrtc.ICECandidate) {
			if c == nil {
				return
			}
			_ = ws.WriteJSON(Msg{Type: "candidate", Candidate: c.ToJSON()})
		})

		pc.OnTrack(func(remote *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
			log.Println("[RTC] OnTrack:", remote.Kind().String(), "codec:", remote.Codec().MimeType)

			// Request keyframes periodically
			go func(ssrc uint32) {
				t := time.NewTicker(2 * time.Second)
				defer t.Stop()
				for range t.C {
					_ = pc.WriteRTCP([]rtcp.Packet{
						&rtcp.PictureLossIndication{MediaSSRC: ssrc},
					})
				}
			}(uint32(remote.SSRC()))

			// Forward RTP (rewrite PT if negotiated)
			for {
				pkt, _, err := remote.ReadRTP()
				if err != nil {
					log.Println("[RTC] ReadRTP ended:", err)
					return
				}

				// Copy packet so we don't mutate internal buffers
				out := &rtp.Packet{
					Header:  pkt.Header,
					Payload: pkt.Payload,
				}
				pt := negotiatedPT.Load()
				if pt != 0 {
					out.PayloadType = uint8(pt)
				}
				_ = echoTrack.WriteRTP(out)
			}
		})

		// ---- Signaling loop ----
		for {
			_, raw, err := ws.ReadMessage()
			if err != nil {
				log.Println("[WS] read:", err)
				return
			}
			var msg Msg
			if err := json.Unmarshal(raw, &msg); err != nil {
				log.Println("json:", err)
				continue
			}

			switch msg.Type {
			case "offer":
				if err := pc.SetRemoteDescription(msg.Offer); err != nil {
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

				// After local description set, payload types are negotiated
				params := echoSender.GetParameters()
				if len(params.Codecs) > 0 {
					negotiatedPT.Store(uint32(params.Codecs[0].PayloadType))
					log.Println("[RTC] negotiated echo PT:", params.Codecs[0].PayloadType)
				} else {
					log.Println("[RTC] WARNING: no negotiated codecs on sender")
				}

				_ = ws.WriteJSON(Msg{Type: "answer", Answer: answer})
				log.Println("[RTC] sent answer")

			case "candidate":
				_ = pc.AddICECandidate(msg.Candidate)
			}
		}
	})

	log.Println("[HTTP] ws on :8765/ws")
	log.Fatal(http.ListenAndServe(":8765", nil))
}

func getenv(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}
