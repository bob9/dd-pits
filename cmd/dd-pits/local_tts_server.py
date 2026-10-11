#!/usr/bin/env python3
"""Local text-to-speech sidecar for FPVTrackside.

Speaks arbitrary text on the venue laptop with no network and no
pre-generation, using Qwen3-TTS through MLX on Apple Silicon.

WHY A SIDECAR, AND WHY IT OWNS PLAYBACK
---------------------------------------
The model runs under MLX, which is Python-only, so some bridge is
unavoidable. Playback lives on this side of it deliberately: generation
produces audio about four times faster than it plays, so if the first chunk
goes to the speaker immediately the rest is always ready before it is needed.
Measured on an M4 Max that is ~74ms from text to sound. Handing whole clips
back to C# instead would mean waiting for generation to finish first — about
2 seconds, which is the difference between a lap call landing on the crossing
and landing on the next corner.

PROTOCOL (line-based over stdin/stdout, so there is no port to configure)
    <- READY                 model loaded and warmed
    -> SPEAK <text>          speak it; blocks the caller until finished
    <- DONE                  playback complete
    -> COMMENT <json>        write a line of colour from race state, then speak it
    <- SAID <text>           what it said (empty if it declined)
    -> STOP                  cut the current line (a higher-priority call)
    -> VOLUME <0-100>
    -> RENDER <text>         render to audio and RETURN it, playing nothing
    -> WRITE <json>          write from a RAW prompt and return the words, speaking nothing
    -> STREAM <text>         render and return audio AS IT IS GENERATED, playing nothing
    <- AUDIO <base64 pcm16>  24kHz mono PCM, for a caller that owns its own bus
    -> PING                  <- PONG
    -> QUIT

Every failure is reported rather than swallowed: if this process dies or
never becomes READY, FPVTrackside falls back to the voice pack or the system
voice, which is what it did before this existed.
"""
import argparse
import base64
import json
import os
import queue
import sys
import threading
import time

# The model produces roughly 12 audio tokens per second of speech. Left
# unbounded it can occasionally loop and emit minutes of noise — observed once
# as 327 seconds of buzzing. On a PA at a race that is unacceptable, so output
# is capped: a runaway becomes a clipped call rather than a stuck one.
#
# Raised from 15s, which was cutting legitimate post-race wraps off mid-sentence
# — a 60-word wrap is roughly 25 seconds of speech, and 15 was chosen when every
# line was a one-liner. The cap is a runaway guard, not a length policy: a
# runaway is minutes long, so 35s still bounds it without truncating real
# commentary.
MAX_SPEECH_SECONDS = 35
MAX_TOKENS = 900


def speech_budget_secs(text):
    """Ceiling for how long TEXT can plausibly take to say.

    The flat cap bounds a runaway, but for a race call the flat cap IS the
    runaway: with a cloned reference the model occasionally misses its
    end-of-speech and babbles, and '10 remaining.' was seen rendering for the
    full 35 seconds — GPU held the whole time, every call behind it starving
    (measured 18-21s to first audio, calls dropped as 'the moment has
    passed'). Speech runs about 14 characters a second; three times that plus
    a 4-second floor forgives slow delivery, pauses and short exclamations,
    while a babble loop on a two-word call is cut in seconds instead of tens
    of them. Long booth wraps still get the full flat cap.
    """
    return min(MAX_SPEECH_SECONDS, max(4.0, len(text) / 14.0 * 3.0))

# Small chunks are what make the first sound arrive quickly. Below ~0.1s there
# is no further gain; above it latency climbs roughly linearly.
# Smaller yields the first fragment sooner, which is what the caller is waiting
# on. 0.05 halves the time before ANY audio exists without changing how fast the
# rest is produced.
STREAM_INTERVAL = 0.05

# Chunks to hold before playback starts.
#
# Starting on the very first chunk sounds crackly: each chunk is only 80ms, and
# if the next one is even slightly late the device runs dry and clicks. That is
# especially likely over Bluetooth, which has its own large and variable buffer.
# Generation runs about four times faster than playback, so a couple of chunks
# of head start is enough to stay ahead for the rest of the line — and costs
# only ~160ms, still far below the ~660ms Bluetooth itself adds.
PREBUFFER_CHUNKS = 3

# Device buffer. "low" asks PortAudio for the smallest it can manage, which is
# what leaves no room to absorb jitter; a modest buffer is steadier and the
# added delay is not perceptible next to the device's own.
DEVICE_LATENCY = 0.15

# How long a gap counts as "quiet enough to re-check the output device".
# Re-checking means a full device re-open, so it is not done between calls in a
# burst — but any pause longer than this picks up a device change.
DEVICE_RECHECK_SECONDS = 2.0

# How many rendered lines to keep. Each is a few seconds of 24kHz mono, so a few
# hundred is a handful of megabytes — and a race day only has a few dozen
# distinct fixed lines anyway.
CACHE_LINES = 300

# Delivery speed is left alone.
#
# The pre-generated pack was tightened by 1.18x, so live speech is a little
# slower for the same words. Applying the same stretch here was tried and made
# it WORSE — measured 2.4s to 2.7s — because streaming hands over one grain at a
# time and a time-stretch needs the whole utterance to align against. Doing it
# properly means buffering the entire line first, which costs more latency than
# the speed-up saves.
SPEECH_SPEED = 1.0


def log(msg):
    print(f"# {msg}", file=sys.stderr, flush=True)


class Speaker:
    def __init__(self, model_id, ref_audio=None, ref_text=None, device=None,
                 render_only=False):
        import numpy as np
        import sounddevice as sd
        from mlx_audio.tts.utils import load_model

        self.np = np
        self.sd = sd

        t0 = time.time()
        self.model = load_model(model_id)
        self.sr = getattr(self.model, "sample_rate", 24000)
        log(f"loaded {model_id} in {time.time() - t0:.1f}s (sr={self.sr})")

        self.ref_audio = ref_audio
        self.ref_text = ref_text
        self.volume = 1.0
        self._stop = threading.Event()

        # Rendered audio, keyed on the exact words.
        #
        # A race day repeats its fixed lines constantly — "Pilots turn on your
        # video transmitters", "Arm your quads...", "Race over" — and there is
        # no reason to synthesise them twice. The start sequence is nothing but
        # these, which is why it felt slow: every heat regenerated the same
        # audio from scratch.
        #
        # Lap calls never repeat, so they are unaffected either way.
        self._cache = {}
        self._cache_order = []

        # Opened once and reused: opening it per call cost ~400ms of device
        # setup, far more than generation itself.
        self.stream = None
        self.device_name = None
        self.forced_device = device
        self.last_write = 0.0

        # A caller that only ever RENDERs plays nothing, so opening an output
        # device would hold one it never writes to. That matters on a venue
        # laptop, where several processes share one speaker and the contention
        # shows up as crackle rather than as an error.
        self.render_only = render_only
        if render_only:
            log("render-only: no audio device opened")
        else:
            self._ensure_stream()
            log(f"audio out: {self.device_name} ({self.stream.latency * 1000:.0f}ms device latency)")

        self._warm()

    def _default_output_name(self):
        """The system's current default output.

        PortAudio caches its device list at initialisation, so it must be
        re-initialised to notice the default has moved — the same trap OpenAL
        has. That tear-down invalidates any open stream, which is why the
        caller closes the stream first.
        """
        try:
            self.sd._terminate()
            self.sd._initialize()
            return self.sd.query_devices(kind="output")["name"]
        except Exception as ex:
            log(f"device query failed: {ex}")
            return None

    def _ensure_stream(self):
        """Open the output stream, following the system default.

        The device is re-checked whenever there has been a quiet gap, which is
        when someone actually changes it — connecting headphones between heats.
        It is NOT checked during a burst of calls, because the check costs a
        full device re-open.

        An earlier version only re-checked when the stream had gone inactive.
        That never fired: PortAudio keeps a stream active between calls, so the
        condition was never true and the output stayed on whatever was default
        at startup — exactly the bug this is meant to fix.
        """
        now = time.time()
        quiet = (now - self.last_write) > DEVICE_RECHECK_SECONDS

        if self.stream is not None and not quiet:
            try:
                if not self.stream.active:
                    self.stream.start()
                return
            except Exception:
                pass        # fall through and rebuild it

        # Close before re-initialising: terminate() invalidates open streams,
        # and using one afterwards raises "Invalid stream pointer".
        if self.stream is not None:
            try:
                self.stream.close()
            except Exception:
                pass
            self.stream = None

        current = self._default_output_name()
        if current != self.device_name and self.device_name is not None:
            log(f"output moved to {current} (was {self.device_name})")

        self.stream = self.sd.OutputStream(
            samplerate=self.sr, channels=1, dtype="float32", latency=DEVICE_LATENCY)
        self.device_name = current
        self.stream.start()

    def _tighten(self, audio):
        """Speeds speech up WITHOUT raising its pitch.

        Overlap-add at the ORIGINAL sample rate: the waveform is advanced faster
        than it is consumed, and each join is aligned to the best-matching
        offset so periodic sounds stay in phase. Simply resampling would play
        faster but lift the pitch into a chipmunk.
        """
        np = self.np
        if SPEECH_SPEED <= 1.001 or len(audio) < 2048:
            return audio

        window = max(64, int(self.sr * 0.03))      # 30ms grains
        overlap = window // 2
        search = max(8, int(self.sr * 0.004))      # +/-4ms alignment
        advance = int(overlap * SPEECH_SPEED)

        out = np.zeros(0, dtype=np.float32)
        read = 0
        fade = np.linspace(0.0, 1.0, overlap, dtype=np.float32)

        while read + window + search < len(audio):
            if len(out) == 0:
                out = audio[read:read + window].copy()
                read += advance
                continue

            tail = out[-overlap:]

            # Align the next grain to whatever best matches what is already
            # written — this is what stops the warble.
            best, best_score = 0, -1e30
            for off in range(-search, search + 1, 4):
                src = read + off
                if src < 0 or src + overlap >= len(audio):
                    continue
                score = float(np.dot(tail, audio[src:src + overlap]))
                if score > best_score:
                    best_score, best = score, off

            frm = read + best
            if frm < 0 or frm + window >= len(audio):
                break

            out[-overlap:] = tail * (1.0 - fade) + audio[frm:frm + overlap] * fade
            out = np.concatenate([out, audio[frm + overlap:frm + window]])
            read += advance

        return out if len(out) else audio

    def _warm(self):
        """One throwaway pass so the first real call pays nothing.

        With a cloned voice this also populates the reference-encoding cache,
        which is the expensive part — ~370ms cold, then free.
        """
        t0 = time.time()
        for _ in self._generate("warm up"):
            pass
        log(f"warmed in {time.time() - t0:.1f}s")

    def _generate(self, text):
        kw = dict(text=text, verbose=False, stream=True,
                  streaming_interval=STREAM_INTERVAL, max_tokens=MAX_TOKENS)
        if self.ref_audio:
            kw["ref_audio"] = self.ref_audio
            kw["ref_text"] = self.ref_text
        return self.model.generate(**kw)

    def speak(self, text, buffered=False):
        if self.render_only:
            # Not an error: a render-only sidecar is doing exactly what it was
            # started for. Say so once rather than fail a request.
            log("render-only: ignoring a request to play audio")
            return
        """Generate and play, returning once the audio has finished.

        buffered renders the WHOLE line before playing a sample of it.

        Streaming keeps latency low, which is what a race call needs. But it
        leaves only a fraction of a second of audio in the device buffer, so any
        stumble in generation drains it and the line breaks up — heard on a
        20-second post-race wrap, which is long enough for that to be likely.
        Commentary is queued and spoken in a gap, so its latency does not
        matter; rendering it in full trades a delay nobody notices for playback
        that cannot stutter.
        """
        self._stop.clear()
        self._ensure_stream()
        t0 = time.time()
        first = None
        played = 0.0
        budget = speech_budget_secs(text)
        capped = False

        # Already rendered: play it straight out. This is what makes a repeated
        # fixed line instant rather than a fresh synthesis.
        cached = self._cache.get(text)
        if cached is not None:
            self.stream.write(cached * self.volume)
            self.last_write = time.time()
            log(f"0ms to first sound (cached), "
                f"{len(cached) / self.sr:.1f}s spoken: {text!r}")
            return

        rendered = []
        pending = []
        buffer_all = buffered

        try:
            for seg in self._generate(text):
                if self._stop.is_set():
                    break
                audio = getattr(seg, "audio", None)
                if audio is None:
                    continue

                chunk = self.np.asarray(audio, dtype=self.np.float32) * self.volume

                # Build a head start before the first write, then stay ahead.
                # Whole-line mode: collect everything, play nothing yet.
                if buffer_all:
                    rendered.append(chunk)
                    played += len(chunk) / self.sr
                    if played > budget:
                        capped = True
                        log(f"output cut at {played:.1f}s (budget {budget:.1f}s): {text!r}")
                        break
                    continue

                if pending is not None:
                    pending.append(chunk)
                    if len(pending) < PREBUFFER_CHUNKS:
                        continue
                    chunk = self.np.concatenate(pending)
                    pending = None

                if first is None:
                    first = (time.time() - t0) * 1000

                self.stream.write(chunk)
                self.last_write = time.time()
                rendered.append(chunk)
                played += len(chunk) / self.sr

                if played > budget:
                    capped = True
                    log(f"output cut at {played:.1f}s (budget {budget:.1f}s): {text!r}")
                    break

            # Whole-line mode: one continuous write, so it cannot stutter.
            if buffer_all and rendered:
                whole = self.np.concatenate(rendered)
                first = (time.time() - t0) * 1000
                self.stream.write(whole)
                self.last_write = time.time()

            # A line shorter than the pre-buffer never reached the write above.
            if pending and not buffer_all:
                chunk = self.np.concatenate(pending)
                if first is None:
                    first = (time.time() - t0) * 1000
                self.stream.write(chunk)
                self.last_write = time.time()
                played += len(chunk) / self.sr
        except Exception as ex:                     # never take the process down
            log(f"speak failed: {type(ex).__name__}: {ex}")
            return

        # Only a line that finished is worth keeping — a cut-off one would
        # replay truncated forever.
        if rendered and not self._stop.is_set() and not capped:
            self._remember(text, self.np.concatenate(rendered) / max(self.volume, 0.01))

        if first is not None:
            log(f"{first:.0f}ms to first sound, {played:.1f}s spoken: {text!r}")

    def _remember(self, text, audio):
        """Keeps rendered audio, bounded so a long meeting cannot grow forever."""
        if text in self._cache:
            return

        self._cache[text] = audio
        self._cache_order.append(text)

        while len(self._cache_order) > CACHE_LINES:
            oldest = self._cache_order.pop(0)
            self._cache.pop(oldest, None)

    def stop(self):
        self._stop.set()
        # A render-only speaker has no playback stream — the flag alone is the
        # abort there (the generation loops check it). Touching a None stream
        # logged "stop: 'NoneType' object has no attribute 'abort'" on every
        # cancelled render.
        if self.stream is None:
            return
        try:
            self.stream.abort()
            self.stream.start()
        except Exception as ex:
            log(f"stop: {ex}")

    def close(self):
        try:
            self.stream.stop()
            self.stream.close()
        except Exception:
            pass


def _release_gpu_pools():
    """Hands MLX's scratch buffers back after a render.

    Without this the sidecar GROWS with every render and never shrinks: MLX
    pools GPU buffers per allocation size, the booth's 20-second wraps are the
    largest allocations in the system, and the pool keeps every size class it
    has ever needed. Measured live: one sidecar at 6.7GB resident while its two
    siblings sat at 1.1GB, the machine 6GB into swap, and every call crackling
    -- the classic sound of the audio path being paged.

    The model weights are untouched; this only releases scratch. The next
    render re-allocates what it needs, which costs milliseconds against the
    swap death it prevents."""
    try:
        import mlx.core as mx
        if hasattr(mx, "clear_cache"):
            mx.clear_cache()
        elif hasattr(mx, "metal") and hasattr(mx.metal, "clear_cache"):
            mx.metal.clear_cache()
    except Exception:
        pass


def stream_pcm(speaker, text, emit):
    """Render text, handing back each fragment AS SOON as it exists.

    The difference from render_pcm is entirely about WHEN, not what. Generation
    runs at roughly real time, so returning only the finished utterance puts a
    two-second call two seconds behind the lap it describes — which on a race
    day is the difference between a call and a correction.

    The caller can start playing the first fragment while the rest is still
    being made, so the delay becomes the time to the FIRST fragment (~100ms)
    rather than the length of the whole call.
    """
    import numpy as np

    cached = speaker._cache.get(text)
    if cached is not None:
        # Already rendered: hand it over in one piece. Splitting it up would
        # add latency to the one case that has none.
        emit((cached * 32767).astype(np.int16).tobytes())
        return

    # This path had NO cap at all: a runaway streamed up to the token limit
    # (~75 seconds of babble), PLAYED it, and cached it — and this is the
    # path every live race call takes.
    budget = speech_budget_secs(text)
    # A STOP can arrive mid-render: the caller has decided the call is dead
    # (racing started, the moment passed) and the GPU it holds is needed for
    # the live calls. Without this check the generation ran to completion no
    # matter what the caller did — cancelling only abandoned the WAIT.
    speaker._stop.clear()
    chunks = []
    emitted = 0.0
    capped = False
    for seg in speaker._generate(text):
        if speaker._stop.is_set():
            capped = True
            log(f"stream aborted by STOP after {emitted:.1f}s: {text!r}")
            break
        audio = getattr(seg, "audio", None)
        if audio is None:
            continue
        a = np.asarray(audio, dtype=np.float32).reshape(-1)
        chunks.append(a)
        emit((np.clip(a, -1.0, 1.0) * 32767).astype(np.int16).tobytes())
        emitted += len(a) / speaker.sr
        if emitted > budget:
            capped = True
            log(f"runaway cut at {emitted:.1f}s (budget {budget:.1f}s for "
                f"{len(text)} chars): {text!r}")
            break

    # Cached whole, so a repeat of a fixed line costs nothing next time.
    # NEVER a capped one — that would replay the babble forever.
    if chunks and not capped:
        speaker._cache[text] = np.concatenate(chunks)
        speaker._cache_order.append(text)
        # Capped by BYTES, not entries: 200 slots of two-second lap calls is
        # small, but 200 slots of twenty-second wraps is over a gigabyte of
        # float32 sitting in a process that is already fighting for RAM.
        total = sum(a.nbytes for a in speaker._cache.values())
        while speaker._cache_order and total > 64 * 1024 * 1024:
            dropped = speaker._cache.pop(speaker._cache_order.pop(0), None)
            if dropped is not None:
                total -= dropped.nbytes
        while len(speaker._cache_order) > 200:
            speaker._cache.pop(speaker._cache_order.pop(0), None)
    _release_gpu_pools()


def render_pcm(speaker, text):
    """Renders text to PCM and returns it, WITHOUT playing.

    For a caller that owns its own audio bus — dd-pits mixes race calls and
    commentary into one prioritised stream, so it wants the samples, not the
    speaker. Playing here would put two programs on one output, which is the
    contention this exists to remove.
    """
    import numpy as np

    cached = speaker._cache.get(text)
    if cached is not None:
        return (cached * 32767).astype(np.int16).tobytes()

    budget = speech_budget_secs(text)
    speaker._stop.clear()
    chunks = []
    played = 0.0
    capped = False
    for seg in speaker._generate(text):
        if speaker._stop.is_set():
            capped = True
            log(f"render aborted by STOP after {played:.1f}s: {text!r}")
            break
        audio = getattr(seg, "audio", None)
        if audio is None:
            continue
        chunk = np.asarray(audio, dtype=np.float32)
        chunks.append(chunk)
        played += len(chunk) / speaker.sr
        if played > budget:
            capped = True
            log(f"render cut at {played:.1f}s (budget {budget:.1f}s for "
                f"{len(text)} chars): {text!r}")
            break

    if not chunks:
        return b""

    whole = np.concatenate(chunks)
    # A capped render is babble past the words — caching it would replay the
    # babble forever (this path DID cache them).
    if not capped:
        speaker._remember(text, whole)
    _release_gpu_pools()
    return (np.clip(whole, -1.0, 1.0) * 32767).astype(np.int16).tobytes()


class Commentator:
    """Writes a line of colour about what is happening in the race.

    Deliberately small and thinking-disabled. Qwen3 reasons before answering by
    default, which cost 700-2000 tokens on a one-line call and several seconds
    with it — for filler between timing calls that is the whole budget. With
    thinking off a line comes back in about 300ms.

    This never decides WHEN to speak. The race calls own the bus; C# offers a
    moment only when nothing else is queued, and anything said here is cut the
    moment a real call arrives.
    """

    def __init__(self, model_id):
        """Starts loading in the BACKGROUND and returns immediately.

        Speech must not wait on this. A large writer takes minutes to load, and
        loading it inline meant the whole generator was not READY in time — so
        FPVTrackside gave up on live speech and every race call fell back to the
        voice pack. The calls are the job; commentary is colour and can arrive
        late.
        """
        self.model_id = model_id
        self.model = None
        self.tok = None
        self.sampler = None
        self._ready = threading.Event()

        threading.Thread(target=self._load, name="commentary model", daemon=True).start()

    def _load(self):
        try:
            from mlx_lm import load
            from mlx_lm.sample_utils import make_sampler

            t0 = time.time()
            self.model, self.tok = load(self.model_id)
            self.sampler = make_sampler(temp=0.85)
            self._ready.set()
            log(f"commentary model {self.model_id} ready in {time.time() - t0:.1f}s")
        except Exception as ex:
            log(f"commentary model failed to load: {type(ex).__name__}: {ex}")

        # Recent lines, so it does not repeat itself. The race data barely
        # changes between two calls, and without this the model says nearly the
        # same thing every time — which is what makes a booth sound automated.
        self.recent = []

    def raw(self, prompt, max_tokens=320):
        """Generate from a prompt EXACTLY as given.

        Deliberately unwrapped, unlike write(). A caller using this has already
        built the prompt it wants — the race format, the beat brief, its own
        don't-repeat transcript — and quietly wrapping that in a second prompt
        would put two sets of instructions in front of the model and let the
        local path drift from the cloud one it is meant to substitute for.
        """
        from mlx_lm import generate

        if not self._ready.is_set():
            # Still loading. Answer empty rather than block: the caller has a
            # fallback, and a line that lands minutes late is not commentary.
            log("chat model still loading — skipping this request")
            return ""

        if not prompt.strip():
            return ""

        msgs = [{"role": "user", "content": prompt}]
        text = self.tok.apply_chat_template(
            msgs, add_generation_prompt=True, tokenize=False,
            # Thinking is the whole latency cost — benchmarked at seconds of
            # reasoning for a call that is eighty tokens long. A race call is
            # phrasing, not deduction: the context already states the order and
            # the gaps.
            enable_thinking=False)

        try:
            out = generate(self.model, self.tok, prompt=text,
                           max_tokens=max_tokens, sampler=self.sampler,
                           verbose=False)
        except Exception as ex:
            log(f"write failed: {type(ex).__name__}: {ex}")
            return ""
        finally:
            # The WRITER was the whale all along. The GPU-pool release went
            # into the TTS paths while this path — a 14B whose every wrap
            # grows the KV and scratch pools — kept everything it ever
            # allocated: measured at 6.7GB, then 10, then 15 across a race
            # day, with the machine 11GB into swap and every call crackling.
            # Weights stay; scratch goes; the next write re-allocates in
            # milliseconds.
            _release_gpu_pools()

        return out.strip()

    def write(self, context, style="", max_words=16):
        from mlx_lm import generate

        # Still loading — say nothing rather than block. A line that arrives
        # minutes late is not commentary.
        if not self._ready.is_set():
            log("commentary model still loading — skipping this line")
            return ""

        recent = ""
        if self.recent:
            said = "\n".join(f"- {r}" for r in self.recent[-6:])
            recent = f"\n\nYou ALREADY said these — do not repeat them or their shape:\n{said}"

        # A wrap needs room for several sentences; a mid-race line must be one.
        shape = ("Several short sentences are fine — make it sound spoken, not written.\n"
                 if max_words > 30 else "Write ONE short spoken line.\n")

        prompt = (
            f"You are an FPV drone-racing commentator filling a gap between timing calls.\n"
            f"{style}\n\n"
            f"{context}{recent}\n\n"
            f"Write at most {max_words} words.\n"
            f"{shape}"
            "RULES:\n"
            "- Use ONLY the numbers shown above, exactly as written. Every figure there is "
            "seconds or a lap count.\n"
            "- Do NOT invent statistics, units, or comparisons that are not in the data. "
            "There is no such thing as laps per mile, average speed, or percentage pace here.\n"
            "- If you are not certain what a number means, talk about the pilots and positions "
            "instead of quoting it.\n"
            "- No emoji, no stage directions, no quotes."
        )

        msgs = [{"role": "user", "content": prompt}]
        text = self.tok.apply_chat_template(
            msgs, add_generation_prompt=True, tokenize=False, enable_thinking=False)

        try:
            # Roughly 2 tokens per word, with headroom so a sentence is never
            # cut mid-word.
            out = generate(self.model, self.tok, prompt=text,
                           max_tokens=max(48, max_words * 3),
                           sampler=self.sampler, verbose=False)
        except Exception as ex:
            log(f"commentary failed: {type(ex).__name__}: {ex}")
            return ""

        line = " ".join(out.strip().split())
        for ch in ('"', "*", "\u201c", "\u201d"):
            line = line.replace(ch, "")

        # A model that ignores the word limit is worse than silence here: a long
        # line risks still talking when the next lap lands. The allowance is
        # generous rather than exact, so a sentence is not chopped mid-clause.
        limit = int(max_words * 1.5)
        if len(line.split()) > limit:
            line = " ".join(line.split()[:limit])

        if line:
            self.recent.append(line)
            del self.recent[:-12]
        return line


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="mlx-community/Qwen3-TTS-12Hz-0.6B-Base-bf16")
    ap.add_argument("--ref-audio", default=None, help="reference wav for voice cloning")
    ap.add_argument("--ref-text", default=None, help="transcript of the reference wav")
    ap.add_argument("--device", default=None, help="output device name or index")
    ap.add_argument("--llm", default=None,
                    help="chat model for between-call commentary; omit to disable")
    ap.add_argument("--render-only", action="store_true",
                    help="never play audio: answer RENDER and open no output device. "
                         "For a caller that mixes the samples itself.")
    ap.add_argument("--llm-only", action="store_true",
                    help="load ONLY the chat model: no TTS weights, no audio device. "
                         "For a caller that wants WRITE and renders its audio elsewhere.")
    args = ap.parse_args()

    if args.ref_audio and not args.ref_text:
        log("--ref-audio needs --ref-text (the words spoken in the sample)")
        sys.exit(2)
    if args.ref_audio and not os.path.exists(args.ref_audio):
        log(f"reference audio not found: {args.ref_audio}")
        sys.exit(2)

    # In llm-only mode the TTS weights are never loaded and no audio device is
    # opened. A caller that renders its audio elsewhere would otherwise hold a
    # device it never writes to — and on a venue laptop that device is the one
    # thing everything else is contending for.
    speaker = None
    if not args.llm_only:
        try:
            speaker = Speaker(args.model, args.ref_audio, args.ref_text, args.device,
                              render_only=args.render_only)
        except Exception as ex:
            log(f"startup failed: {type(ex).__name__}: {ex}")
            sys.exit(1)
    elif not args.llm:
        log("--llm-only needs --llm (there is nothing else to load)")
        sys.exit(2)

    commentator = None
    if args.llm:
        try:
            commentator = Commentator(args.llm)
        except Exception as ex:
            # Commentary is optional colour; losing it must not cost the calls.
            log(f"commentary unavailable: {type(ex).__name__}: {ex}")

    # Speaking happens on its own thread so STOP can arrive mid-line.
    work = queue.Queue()

    def worker():
        while True:
            item = work.get()
            if item is None:
                break

            kind, payload = item
            if kind == "stream":
                try:
                    def emit(pcm):
                        print("CHUNK " + base64.b64encode(pcm).decode("ascii"), flush=True)
                    stream_pcm(speaker, payload, emit)
                except Exception as ex:
                    log(f"stream failed: {type(ex).__name__}: {ex}")
                # Always terminated, even on failure: a caller waiting for the
                # end of a stream that never ends would hold its cue open and
                # the bus would sit on silence.
                print("STREAMEND", flush=True)

            elif kind == "write":
                text = ""
                if commentator is not None:
                    try:
                        req = json.loads(payload)
                        text = commentator.raw(req.get("prompt", ""),
                                               int(req.get("max_tokens", 320)))
                    except Exception as ex:
                        log(f"write: {type(ex).__name__}: {ex}")
                # One line out, always — a caller blocked on a reply that never
                # comes is worse than an empty answer it can fall back from.
                print("WROTE " + text.replace("\n", " "), flush=True)

            elif kind == "render":
                try:
                    pcm = render_pcm(speaker, payload)
                except Exception as ex:
                    log(f"render failed: {type(ex).__name__}: {ex}")
                    pcm = b""
                print("AUDIO " + base64.b64encode(pcm).decode(), flush=True)
            elif kind in ("speak", "speakb"):
                speaker.speak(payload, buffered=(kind == "speakb"))
                print("DONE", flush=True)
            else:
                line = ""
                if commentator is not None:
                    ctx = {}
                    try:
                        ctx = json.loads(payload)
                        line = commentator.write(ctx.get("context", ""),
                                                 ctx.get("style", ""),
                                                 int(ctx.get("max_words", 16)))
                    except Exception as ex:
                        log(f"commentary: {type(ex).__name__}: {ex}")
                # The caller may want the WORDS only, to queue for a gap of its
                # own choosing rather than have the pipe taken now.
                if line and ctx.get("speak", True):
                    speaker.speak(line)
                print("SAID " + line, flush=True)

    threading.Thread(target=worker, daemon=True).start()

    if args.llm_only and commentator is not None:
        # Background loading exists so the VOICE is usable while the chat model
        # arrives. With no voice here, READY before the model is loaded only
        # buys the caller an empty first answer.
        commentator._ready.wait(timeout=600)

    print("READY", flush=True)

    for line in sys.stdin:
        # rstrip("\r\n"), not just "\n": the caller may be a Windows process,
        # where WriteLine sends CRLF. A surviving carriage return rides into the
        # spoken text and the cache key, where it is invisible and breaks both.
        line = line.rstrip("\r\n")
        if not line:
            continue

        cmd, _, arg = line.partition(" ")
        cmd = cmd.upper()

        if cmd == "SPEAK":
            work.put(("speak", arg))
        elif cmd == "SPEAKB":
            # Buffered: render fully, then play. Used for commentary, which is
            # long enough that streaming can stutter and is never latency
            # critical.
            work.put(("speakb", arg))
        elif cmd == "COMMENT":
            work.put(("comment", arg))
        elif cmd == "STREAM":
            work.put(("stream", arg))
        elif cmd == "WRITE":
            work.put(("write", arg))
        elif cmd == "STOP":
            speaker.stop()
            # Drop anything queued behind it — a stop means the call is stale.
            while not work.empty():
                try:
                    dropped, _ = work.get_nowait()
                    if dropped in ("speak", "speakb"):
                        print("DONE", flush=True)
                    elif dropped == "render":
                        print("AUDIO ", flush=True)
                    else:
                        print("SAID ", flush=True)
                except queue.Empty:
                    break
        elif cmd == "VOLUME":
            try:
                speaker.volume = max(0, min(100, int(arg))) / 100.0
            except ValueError:
                pass
        elif cmd == "RENDER":
            work.put(("render", arg))
        elif cmd == "PING":
            print("PONG", flush=True)
        elif cmd == "QUIT":
            break
        else:
            log(f"unknown command: {cmd}")

    work.put(None)
    if speaker is not None:
        speaker.close()


if __name__ == "__main__":
    main()
