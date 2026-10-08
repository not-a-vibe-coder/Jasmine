"""Jasmine's Telegram caller.

Telegram bots cannot place voice calls, so this small service logs into a normal
Telegram account (Jasmine's "phone") and does the calling for her:

  POST /call {call_id, user, audio_path}   dial @user (or a numeric id), play the opening clip
  ...then for every phrase the other person says, POST it to the Go bot at
  JASMINE_URL/calls/tg/event and play the clip it answers with.

It runs next to the Go bot in the same container (see docker-entrypoint.sh) and only
listens on localhost. Both sides authenticate with CALLER_SECRET.
"""

import array
import asyncio
import base64
import io
import logging
import math
import os
import time
import wave

from aiohttp import ClientSession, ClientTimeout, web
from pytgcalls import PyTgCalls, filters
from pytgcalls.exceptions import CallBusy, CallDeclined, CallDiscarded, TimedOutAnswer
from pytgcalls.types import (
    AudioQuality,
    CallConfig,
    ChatUpdate,
    Device,
    Direction,
    MediaStream,
    RecordStream,
    StreamEnded,
    StreamFrames,
)
from telethon import TelegramClient
from telethon.sessions import StringSession

logging.basicConfig(level=logging.INFO, format="%(asctime)s [Caller] %(message)s")
log = logging.getLogger("caller")
logging.getLogger("telethon").setLevel(logging.WARNING)

API_ID = int(os.environ["TG_API_ID"])
API_HASH = os.environ["TG_API_HASH"]
SESSION = os.environ["TG_CALLER_SESSION"]
SECRET = os.environ["CALLER_SECRET"]
PORT = int(os.environ.get("CALLER_PORT", "8091"))
JASMINE_URL = os.environ.get("JASMINE_URL", f"http://127.0.0.1:{os.environ.get('PORT', '8080')}")

QUALITY = AudioQuality.HIGH  # what we receive: 48 kHz stereo s16le
RATE, CHANNELS = QUALITY.value
SPEECH_RMS = float(os.environ.get("CALLER_SPEECH_RMS", "0.02"))  # 0..1 loudness that counts as talking
END_OF_PHRASE = 1.1  # seconds of quiet that end a phrase
MIN_PHRASE = 0.4  # ignore coughs and clicks
MAX_PHRASE = 25.0
SILENCE_PROMPT = 14.0  # nobody talks for this long -> ask the bot what to do
MAX_CALL = 6 * 60

client = TelegramClient(StringSession(SESSION), API_ID, API_HASH)
calls = PyTgCalls(client)
http: ClientSession | None = None


class Call:
    def __init__(self, call_id: str, chat_id: int):
        self.call_id = call_id
        self.chat_id = chat_id
        self.started = time.monotonic()
        self.speaking_until = float("inf")  # while Jasmine talks, ignore the mic (echo)
        self.buffer = bytearray()
        self.voiced = 0.0
        self.quiet = 0.0
        self.last_activity = time.monotonic()
        self.busy = False  # waiting on the bot for a reply
        self.hangup_after_clip = False
        self.ended = False

    @property
    def speaking(self) -> bool:
        return time.monotonic() < self.speaking_until


async def clip_seconds(path: str) -> float:
    """Clip length via ffprobe, so listening resumes even if no stream-end event arrives."""
    try:
        proc = await asyncio.create_subprocess_exec(
            "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path,
            stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL,
        )
        out, _ = await proc.communicate()
        return float(out.strip() or 0) + 0.3
    except Exception:
        return 8.0


active: dict[int, Call] = {}


def stream(path: str) -> MediaStream:
    return MediaStream(path, audio_parameters=AudioQuality.HIGH, video_flags=MediaStream.Flags.IGNORE)


async def post_event(call: Call, kind: str, **extra) -> dict:
    payload = {"call_id": call.call_id, "type": kind, **extra}
    try:
        async with http.post(
            f"{JASMINE_URL}/calls/tg/event",
            json=payload,
            headers={"X-Caller-Secret": SECRET},
            timeout=ClientTimeout(total=40),
        ) as resp:
            if resp.status != 200:
                log.warning("event %s for %s got HTTP %s", kind, call.call_id, resp.status)
                return {}
            return await resp.json()
    except Exception as e:  # the bot restarting shouldn't crash a call
        log.warning("event %s for %s failed: %s", kind, call.call_id, e)
        return {}


async def say(call: Call, reply: dict) -> None:
    """Play the bot's answer, or hang up if it says the conversation is over."""
    path = reply.get("audio_path")
    call.hangup_after_clip = bool(reply.get("hangup"))
    if path and not call.ended:
        call.speaking_until = time.monotonic() + await clip_seconds(path)
        try:
            await calls.play(call.chat_id, stream(path))
            return
        except Exception as e:
            log.warning("playing reply on %s failed: %s", call.call_id, e)
    call.speaking_until = 0
    if call.hangup_after_clip:
        await hang_up(call, "completed")


async def hang_up(call: Call, reason: str) -> None:
    if call.ended:
        return
    call.ended = True
    active.pop(call.chat_id, None)
    try:
        await calls.leave_call(call.chat_id)
    except Exception:
        pass
    await post_event(call, "ended", reason=reason)


async def dial(call_id: str, user: str, audio_path: str) -> None:
    peer = await client.get_input_entity(int(user) if user.lstrip("-").isdigit() else user)
    chat_id = getattr(peer, "user_id", None)
    if not chat_id:
        raise ValueError("can only call users")
    if chat_id in active:
        raise RuntimeError("already on a call with them")
    call = Call(call_id, chat_id)
    active[chat_id] = call

    async def run():
        try:
            # Blocks until they pick up (or decline / time out after 45s).
            await calls.play(chat_id, stream(audio_path), CallConfig(timeout=45))
        except (CallDeclined, CallDiscarded):
            return await finish_unanswered(call, "declined")
        except CallBusy:
            return await finish_unanswered(call, "busy")
        except TimedOutAnswer:
            return await finish_unanswered(call, "no-answer")
        except Exception as e:
            log.exception("call %s failed", call_id)
            return await finish_unanswered(call, f"error: {e}")
        log.info("call %s connected", call_id)
        call.speaking_until = time.monotonic() + await clip_seconds(audio_path)
        call.last_activity = time.monotonic()
        await post_event(call, "answered")
        try:
            await calls.record(chat_id, RecordStream(True, QUALITY))
        except Exception as e:
            log.warning("could not listen on %s: %s", call_id, e)

    asyncio.create_task(run())


async def finish_unanswered(call: Call, reason: str) -> None:
    log.info("call %s not answered: %s", call.call_id, reason)
    call.ended = True
    active.pop(call.chat_id, None)
    try:
        await calls.leave_call(call.chat_id)
    except Exception:
        pass
    await post_event(call, "failed", reason=reason)


def rms(pcm: bytes) -> float:
    samples = array.array("h", pcm[: len(pcm) - len(pcm) % 2])
    if not samples:
        return 0.0
    return math.sqrt(sum(s * s for s in samples) / len(samples)) / 32768.0


def to_wav(pcm: bytes) -> bytes:
    out = io.BytesIO()
    with wave.open(out, "wb") as w:
        w.setnchannels(CHANNELS)
        w.setsampwidth(2)
        w.setframerate(RATE)
        w.writeframes(pcm)
    return out.getvalue()


@calls.on_update(filters.stream_frame(Direction.INCOMING, Device.MICROPHONE))
async def on_audio(_, update: StreamFrames):
    call = active.get(update.chat_id)
    if call is None or call.ended or call.speaking or call.busy:
        return
    for frame in update.frames:
        pcm = frame.frame
        seconds = len(pcm) / (2 * CHANNELS * RATE)
        loud = rms(pcm) >= SPEECH_RMS
        if loud:
            call.buffer += pcm
            call.voiced += seconds
            call.quiet = 0.0
            call.last_activity = time.monotonic()
        elif call.buffer:
            call.buffer += pcm
            call.quiet += seconds
        phrase_done = call.buffer and (call.quiet >= END_OF_PHRASE or call.voiced >= MAX_PHRASE)
        if phrase_done:
            pcm_phrase, voiced = bytes(call.buffer), call.voiced
            call.buffer, call.voiced, call.quiet = bytearray(), 0.0, 0.0
            if voiced >= MIN_PHRASE:
                call.busy = True
                asyncio.create_task(send_phrase(call, pcm_phrase))
                return


async def send_phrase(call: Call, pcm: bytes) -> None:
    try:
        reply = await post_event(call, "speech", audio=base64.b64encode(to_wav(pcm)).decode())
        await say(call, reply)
    finally:
        call.busy = False
        call.last_activity = time.monotonic()


@calls.on_update(filters.stream_end())
async def on_clip_done(_, update: StreamEnded):
    call = active.get(update.chat_id)
    if call is None or update.device not in (Device.MICROPHONE, None):
        return
    call.speaking_until = 0
    call.last_activity = time.monotonic()
    if call.hangup_after_clip:
        await hang_up(call, "completed")


@calls.on_update(filters.chat_update(ChatUpdate.Status.DISCARDED_CALL | ChatUpdate.Status.BUSY_CALL))
async def on_hangup(_, update: ChatUpdate):
    call = active.get(update.chat_id)
    if call is not None and not call.ended:
        log.info("call %s hung up by the other side", call.call_id)
        call.ended = True
        active.pop(call.chat_id, None)
        await post_event(call, "ended", reason="completed")


async def watchdog():
    """Prompt on long silences and cap call length."""
    while True:
        await asyncio.sleep(2)
        now = time.monotonic()
        for call in list(active.values()):
            if call.ended:
                continue
            if now - call.started > MAX_CALL or (call.hangup_after_clip and not call.speaking):
                await hang_up(call, "completed")
            elif not call.speaking and not call.busy and now - call.last_activity > SILENCE_PROMPT:
                call.busy = True
                try:
                    await say(call, await post_event(call, "silence"))
                finally:
                    call.busy = False
                    call.last_activity = time.monotonic()


async def handle_call(request: web.Request) -> web.Response:
    if request.headers.get("X-Caller-Secret") != SECRET:
        return web.Response(status=403, text="forbidden")
    body = await request.json()
    try:
        await dial(body["call_id"], str(body["user"]), body["audio_path"])
    except Exception as e:
        log.warning("dial %s failed: %s", body.get("user"), e)
        return web.Response(status=400, text=str(e))
    return web.json_response({"ok": True})


async def health(_: web.Request) -> web.Response:
    return web.json_response({"ok": True, "active_calls": len(active)})


async def main():
    global http
    http = ClientSession()
    await calls.start()
    me = await client.get_me()
    log.info("logged in as %s (@%s); listening on 127.0.0.1:%d", me.first_name, me.username, PORT)
    app = web.Application(client_max_size=1 << 20)
    app.router.add_post("/call", handle_call)
    app.router.add_get("/health", health)
    runner = web.AppRunner(app)
    await runner.setup()
    await web.TCPSite(runner, "127.0.0.1", PORT).start()
    asyncio.create_task(watchdog())
    await asyncio.Event().wait()


if __name__ == "__main__":
    client.loop.run_until_complete(main())
