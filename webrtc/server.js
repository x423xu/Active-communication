const WebSocket = require("ws");
const { RTCPeerConnection } = require("werift");

const WS_PORT = 8765;

const TURN_HOST = process.env.TURN_HOST || "10.37.0.225";
const TURN_USER = process.env.TURN_USER || "test";
const TURN_PASS = process.env.TURN_PASS || "test123";

const iceServers = [
    {
        urls: [`turn:${TURN_HOST}:3478?transport=tcp`],
        username: TURN_USER,
        credential: TURN_PASS,
    },
];

process.on("uncaughtException", (err) => console.error("[FATAL] uncaughtException:", err));
process.on("unhandledRejection", (err) => console.error("[FATAL] unhandledRejection:", err));

function subscribeIfPresent(obj, fieldName, cb) {
    const ev = obj && obj[fieldName];
    if (ev && typeof ev.subscribe === "function") {
        ev.subscribe(cb);
        console.log(`[RTC] subscribed to ${fieldName}`);
        return true;
    }
    console.log(`[RTC] ${fieldName} not available`);
    return false;
}

const wss = new WebSocket.Server({ port: WS_PORT }, () => {
    console.log(`[WS] listening on :${WS_PORT}`);
    console.log(`[ICE] TURN = turn:${TURN_HOST}:3478?transport=tcp (TCP-only)`);
});

wss.on("connection", (ws) => {
    console.log("[WS] client connected");

    let pc = null;
    let remoteDescSet = false;
    let pendingCandidates = [];

    ws.on("close", (code, reason) => {
        console.log("[WS] closed", { code, reason: reason?.toString?.() });
        try { pc && pc.close && pc.close(); } catch { }
    });

    ws.on("error", (e) => console.log("[WS] error:", e?.message || e));

    ws.on("message", async (raw) => {
        try {
            const msg = JSON.parse(raw.toString());
            console.log("[WS] recv type:", msg.type);

            if (msg.type === "offer") {
                pc = new RTCPeerConnection({ iceServers });

                // ---- IMPORTANT: create a video transceiver in sendrecv mode ----
                // We will receive RTP from browser, then echo RTP back out.
                const videoTransceiver = pc.addTransceiver("video", "sendrecv");

                // When track arrives, forward RTP back to the same transceiver.
                videoTransceiver.onTrack.subscribe((track, transceiver) => {
                    console.log("[RTC] got video track, ssrc=", track.ssrc);


                    let inPkts = 0;
                    let outPkts = 0;

                    setInterval(() => {
                        console.log(`[RTP] in=${inPkts}/s out=${outPkts}/s ssrc=${track.ssrc}`);
                        inPkts = 0;
                        outPkts = 0;
                    }, 1000);

                    track.onReceiveRtp.subscribe((rtp) => {
                        inPkts++;
                        try {
                            transceiver.sendRtp(rtp);
                            outPkts++;
                        } catch (e) {
                            console.log("[RTP] sendRtp error:", e?.message || e);
                        }
                    });
                });

                // Server -> client ICE candidates (browser-compatible)
                subscribeIfPresent(pc, "onIceCandidate", (c) => {
                    if (!c) return;
                    const init = (typeof c.toJSON === "function") ? c.toJSON() : c;
                    ws.send(JSON.stringify({ type: "candidate", candidate: init }));
                });

                // Optional logs
                subscribeIfPresent(pc, "iceConnectionStateChange", (s) => console.log("[RTC] iceConnectionState:", s));
                subscribeIfPresent(pc, "connectionStateChange", (s) => console.log("[RTC] connectionState:", s));

                // Apply offer
                await pc.setRemoteDescription(msg.offer);
                remoteDescSet = true;

                // Flush early candidates (if any)
                if (pendingCandidates.length > 0) {
                    console.log(`[RTC] flushing ${pendingCandidates.length} early candidates`);
                    for (const c of pendingCandidates) {
                        try { await pc.addIceCandidate(c); } catch (e) {
                            console.log("[RTC] addIceCandidate(early) failed:", e?.message || e);
                        }
                    }
                    pendingCandidates = [];
                }

                // Create + send answer
                const answer = await pc.createAnswer();
                await pc.setLocalDescription(answer);
                ws.send(JSON.stringify({ type: "answer", answer }));
                console.log("[RTC] sent answer");
                return;
            }

            if (msg.type === "candidate") {
                if (!pc) {
                    pendingCandidates.push(msg.candidate);
                    return;
                }
                if (!remoteDescSet) {
                    pendingCandidates.push(msg.candidate);
                    return;
                }
                try {
                    await pc.addIceCandidate(msg.candidate);
                } catch (e) {
                    console.log("[RTC] addIceCandidate failed:", e?.message || e);
                }
                return;
            }
        } catch (err) {
            console.error("[WS] handler error:", err);
        }
    });

    // Keepalive ping (helps with VPN/proxies)
    const t = setInterval(() => {
        if (ws.readyState === WebSocket.OPEN) ws.ping();
    }, 15000);
    ws.on("close", () => clearInterval(t));
});
