import asyncio
import json
import os
import struct
import time
import websockets

SNAPSHOT_PERIOD_SEC = int(os.environ.get("SNAPSHOT_PERIOD_SEC", "3"))

def pack_msg(msg_type: int, payload: bytes) -> bytes:
    return bytes([msg_type]) + struct.pack(">I", len(payload)) + payload

class SessionState:
    def __init__(self, session_id: str, snapshot_period: int):
        self.session_id = session_id
        self.snapshot_period = snapshot_period
        self.frame_count = 0
        self.last_frame_bytes = 0
        self.start_t = time.time()
        self._last_report_t = self.start_t
        self._last_report_frames = 0

    def on_frame(self, nbytes: int):
        self.frame_count += 1
        self.last_frame_bytes = nbytes

    def fps_estimate(self):
        now = time.time()
        dt = max(1e-6, now - self._last_report_t)
        df = self.frame_count - self._last_report_frames
        fps = df / dt
        self._last_report_t = now
        self._last_report_frames = self.frame_count
        return fps

async def status_loop(send_q: asyncio.Queue, st: SessionState):
    last_snapshot = time.time()
    while True:
        await asyncio.sleep(1.0)

        # status heartbeat
        fps = st.fps_estimate()
        payload = json.dumps({
            "type": "status",
            "session_id": st.session_id,
            "uptime_sec": round(time.time() - st.start_t, 2),
            "frame_count": st.frame_count,
            "last_frame_bytes": st.last_frame_bytes,
            "fps_est": round(fps, 2),
            "gpu_visible": os.environ.get("NVIDIA_VISIBLE_DEVICES", "(not set)"),
            "note": "placeholder ffgs: receiving frames + sending status"
        }).encode("utf-8")
        await send_q.put(pack_msg(3, payload))

        # periodic snapshot (just a JSON placeholder)
        now = time.time()
        if now - last_snapshot >= st.snapshot_period:
            last_snapshot = now
            snap = json.dumps({
                "type": "snapshot",
                "session_id": st.session_id,
                "frame_count": st.frame_count,
                "note": "placeholder snapshot"
            }).encode("utf-8")
            await send_q.put(pack_msg(2, snap))

async def handler(ws):
    session_id = None
    snapshot_period = SNAPSHOT_PERIOD_SEC
    send_q = asyncio.Queue()
    status_task = None

    async def sender():
        while True:
            msg = await send_q.get()
            await ws.send(msg)

    sender_task = asyncio.create_task(sender())

    try:
        async for message in ws:
            if isinstance(message, str):
                try:
                    m = json.loads(message)
                except Exception:
                    continue

                if m.get("type") == "start":
                    session_id = m.get("session_id", "unknown")
                    snapshot_period = int(m.get("snapshot_period_sec", SNAPSHOT_PERIOD_SEC))
                    st = SessionState(session_id, snapshot_period)
                    await send_q.put(pack_msg(3, json.dumps({
                        "type": "status",
                        "session_id": session_id,
                        "note": f"ffgs start ok, snapshot_period={snapshot_period}"
                    }).encode("utf-8")))

                    if status_task is None:
                        status_task = asyncio.create_task(status_loop(send_q, st))
                    ws.session_state = st  # attach

                elif m.get("type") == "end":
                    await send_q.put(pack_msg(3, json.dumps({
                        "type": "status",
                        "session_id": session_id,
                        "note": "ffgs end received"
                    }).encode("utf-8")))
                    break

            else:
                # binary: first byte is 0x01, rest is JPEG
                if len(message) < 2:
                    continue
                msg_type = message[0]
                if msg_type != 0x01:
                    continue
                jpg = message[1:]

                st = getattr(ws, "session_state", None)
                if st is not None:
                    st.on_frame(len(jpg))

                # optionally send a tiny delta every few frames for proof
                if st is not None and (st.frame_count % 30 == 0):
                    delta = json.dumps({
                        "type": "delta",
                        "session_id": st.session_id,
                        "frame_count": st.frame_count,
                        "note": "placeholder delta every 30 frames"
                    }).encode("utf-8")
                    await send_q.put(pack_msg(1, delta))

    finally:
        if status_task:
            status_task.cancel()
        sender_task.cancel()

async def main():
    print("[FFGS] WS server on 0.0.0.0:9000 (path ignored; main.go uses ws://.../recon)")
    async with websockets.serve(handler, "0.0.0.0", 9000, max_size=64 * 1024 * 1024):
        await asyncio.Future()

if __name__ == "__main__":
    asyncio.run(main())
