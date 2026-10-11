#!/usr/bin/env python3
"""Play raw PCM from stdin on this machine's speakers.

Exists because the HTTP stream is the wrong transport for LOCAL listening.
/booth.wav is right for OBS on another box — it is a radio station and ffmpeg
treats it as one — but ffmpeg fills a network buffer before it plays a sample,
and OBS defaults that to 2 MB. At 24 kHz 16-bit mono the stream is 48 kB/s, so
that buffer is roughly forty-five seconds of audio: the calls are correct and
three-quarters of a minute behind the race.

No protocol here on purpose. dd-pits already mixes every cue into one paced
frame stream; this reads those bytes and writes them to the device. Latency is
one frame plus whatever the device adds.

    local_audio_out.py [--rate 24000] [--device NAME|INDEX]
"""

import argparse
import signal
import sys
import time

# Device buffer, in seconds.
#
# NOT "low". PortAudio's low-latency setting asks for the smallest buffer the
# device can manage, which leaves no room to absorb jitter — and this stream is
# fed by a mixer pumping 100ms frames from another process, so jitter is
# guaranteed. Over Bluetooth, where the device adds its own large and variable
# buffer, the result is audible crackle: the device runs dry between frames.
#
# The TTS sidecar learned this for its own playback and settled on the same
# value; the added delay is imperceptible next to what a headset adds anyway.
# MEASURED, because CoreAudio treats this as a MINIMUM and rounds up steeply —
# on the built-in speakers at 24kHz the curve is nothing like linear:
#
#     request   0.01   0.02   0.03   0.05   0.08   0.10   0.15
#     actual     53ms   74ms  117ms  221ms  251ms  441ms  833ms
#
# The old 0.15 therefore bought an 833ms buffer: three quarters of a second in
# front of every race call, dwarfing the ~110ms the voice takes to reach its
# first sample. It was chosen to stop Bluetooth crackle, and it did — but it was
# treating the symptom.
#
# The crackle is an UNDERRUN, and the buffer only has to cover how late a frame
# can be. The mixer pumps one 100ms frame per 100ms tick, so audio arrives at
# exactly 1x and no lead can ever be built: the buffer is the whole jitter
# margin. What made frames late was the GPU being busy generating commentary
# during a heat, which starves the sidecar — so holding commentary while racing
# (-booth-yield-while-racing, the default) removes the jitter at its source and
# makes a small buffer safe.
#
# 0.03 -> ~117ms here: comfortably more than one frame of margin, and 716ms
# quicker than before. --latency tunes it per venue, and the achieved figure is
# logged on open so the real cost is visible rather than assumed.
DEVICE_LATENCY = 0.03

# Bytes read from the parent per write. One frame at 24kHz 16-bit mono is
# ~4800 bytes, so this stays under a frame and keeps the device fed steadily
# rather than in bursts.
READ_CHUNK = 4096

# How long the bus must be CONTINUOUSLY silent before looking for the default
# output having moved.
#
# It has to be a silence gap, not a timer, because the check is destructive:
# PortAudio only notices a new default if it is re-initialised, and doing that
# invalidates the open stream. So every check costs a re-open, and a re-open
# during speech drops audio mid-word. Long enough to sit between heats rather
# than between calls.
DEVICE_RECHECK_SECONDS = 8.0

# Underruns tolerated before the buffer is doubled, and the ceiling it grows to.
# A handful can happen when a device is waking; a sustained run means the buffer
# is genuinely too small for this output.
UNDERRUNS_BEFORE_GROW = 3
MAX_DEVICE_LATENCY = 0.5


def _device_index(name):
    """PortAudio's index for a device name, or None.

    A Bluetooth device that reconnects keeps its NAME but takes a new index, so
    this is what tells a genuine reconnect from nothing having changed.
    """
    try:
        for i, d in enumerate(sd.query_devices()):
            if d["max_output_channels"] > 0 and d["name"] == name:
                return i
    except Exception:
        pass
    return None


def is_silent(chunk):
    """True when a frame carries no audible signal.

    Used to pick a safe moment to re-open the device. Not exact — it only has
    to avoid interrupting speech, and the bus sends true silence between calls.
    """
    return not any(chunk)


def log(msg):
    # Stderr, prefixed: the parent forwards these into its own log, and stdout
    # carries nothing so it can never be confused with an audio path.
    print(f"# {msg}", file=sys.stderr, flush=True)


# Set by SIGUSR1: the bus has been cleared and whatever is buffered on the
# device must be thrown away rather than played out.
_flush = {"now": False}


def _on_flush(signum, frame):
    _flush["now"] = True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--latency", default=str(DEVICE_LATENCY),
                    help="device buffer: seconds, or 'low'/'high'. See DEVICE_LATENCY.")
    ap.add_argument("--rate", type=int, default=24000)
    ap.add_argument("--channels", type=int, default=1)
    ap.add_argument("--device", default=None,
                    help="output device name or index; omit for the system default")
    # --list-devices exists so the settings page can offer a DROPDOWN of the
    # real outputs instead of a free-text box. Typing a device name fragment
    # from memory is a guessing game, and a typo is indistinguishable from a
    # device that is not working.
    ap.add_argument("--list-devices", action="store_true",
                    help="print the output devices as JSON and exit")
    args = ap.parse_args()

    if args.list_devices:
        import json as _json
        try:
            import sounddevice as _sd
            out = []
            for i, d in enumerate(_sd.query_devices()):
                # Output-capable only: half the list is microphones, and
                # offering those as somewhere to play audio is just noise.
                if d.get("max_output_channels", 0) > 0:
                    out.append({"index": i, "name": d.get("name", "?")})
            try:
                out_default = _sd.query_devices(kind="output")
                default_name = out_default.get("name", "")
            except Exception:
                default_name = ""
            print(_json.dumps({"devices": out, "default": default_name}))
        except Exception as ex:
            print(_json.dumps({"error": str(ex)}))
        return

    try:
        import sounddevice as sd
    except Exception as ex:
        log(f"sounddevice unavailable: {type(ex).__name__}: {ex}")
        return 1

    forced = args.device
    if forced is not None:
        try:
            forced = int(forced)
        except ValueError:
            pass  # a name is equally valid

    def default_output_name():
        """The system's CURRENT default output.

        PortAudio caches its device list at initialisation, so it has to be
        re-initialised to notice the default has moved — otherwise a laptop that
        had built-in speakers as default when this started keeps playing there
        after the operator connects headphones, which is exactly the bug this
        exists to avoid. Tearing down invalidates any open stream, so the caller
        closes first.
        """
        try:
            sd._terminate()
            sd._initialize()
            return sd.query_devices(kind="output")["name"]
        except Exception as ex:
            log(f"device query failed: {ex}")
            return None

    # "low"/"high" are PortAudio's own words; anything else is seconds.
    try:
        _latency = float(args.latency)
    except (TypeError, ValueError):
        _latency = args.latency

    # "base" is what was asked for; "latency" is what we have LEARNED this
    # output needs. They diverge when a starving device grows its buffer, and
    # the base is kept so the growth can be undone when the device changes.
    state = {"stream": None, "name": None, "on_fallback": False,
             "latency": _latency, "base": _latency}

    def current_latency():
        return state["latency"]

    def open_stream():
        if state["stream"] is not None:
            try:
                state["stream"].close()
            except Exception:
                pass
            state["stream"] = None

        # The named device first, the system default as a fallback.
        #
        # A device named on the command line may simply not be connected — the
        # AirPods are in their case. Refusing to play at all in that case was
        # measured as an ENTIRE SESSION of silence with the cause in one line at
        # startup: the sidecar exited and nothing brought it back. Being heard
        # on the built-in speakers is worse than being heard on the AirPods, and
        # far better than not being heard.
        attempts = []
        if forced is not None:
            attempts.append((forced, forced, False))
        dflt = default_output_name()
        attempts.append((dflt, dflt, forced is not None))

        stream = None
        last_err = None
        for target, name, is_fallback in attempts:
            try:
                stream = _open(target, name)
            except Exception as ex:
                last_err = ex
                continue
            if is_fallback and not state["on_fallback"] and state["name"] is None:
                log(f"'{forced}' is not connected — playing on {name} instead, "
                    f"and switching over as soon as it appears")
            state["on_fallback"] = is_fallback
            break
        if stream is None:
            raise last_err if last_err else RuntimeError("no output device")

        # The achieved latency rides along with a device CHANGE.
        #
        # _open reports it only on the first open, to keep the every-silence-gap
        # re-open probe out of the log. That is right for the probe and wrong
        # here: a change of device is already worth a line, and the buffer is the
        # whole reason a call can be generated in 106ms and still sound late.
        # Without it the log said "moved to MacBook Pro Speakers" and left the
        # cost invisible — with the previous device having opened at 1003ms.
        #
        # It is not inherited-and-forgotten either: state["latency"] GROWS on
        # underruns and carries across a move, so a buffer that doubled for a
        # struggling Bluetooth headset follows you to the speakers. Printing the
        # achieved figure is what makes that visible instead of theoretical.
        if state["name"] is not None and name != state["name"]:
            log(f"output moved to {name} (was {state['name']}) — "
                f"{stream.latency * 1000:.0f}ms device latency")
            # A learned buffer belongs to the device that needed it.
            #
            # Growth is triggered by underruns, which are a property of THAT
            # output — a Bluetooth headset starving is not evidence the built-in
            # speakers will. Carrying the grown value over means unplugging the
            # headset leaves you with its buffer in front of every call, on a
            # device that never needed it, and nothing in the log to say so.
            #
            # Reset to what was asked for and let the new device earn its own
            # growth. It takes effect on the next re-open (one silence gap, so
            # under DEVICE_RECHECK_SECONDS) rather than here, because re-opening
            # at this point would drop whatever is being spoken.
            if state["latency"] != state["base"]:
                log(f"buffer was grown to {state['latency']:.2f}s for "
                    f"{state['name']} — back to {state['base']:.2f}s for this device")
                state["latency"] = state["base"]
        elif state["name"] is not None and state.get("index") != _device_index(name):
            # Same NAME, different device: a Bluetooth headset that reconnected.
            # Worth a line, because "audio stopped and nothing in the log" is
            # exactly what made this hard to find.
            log(f"{name} reconnected — reopened on the new device "
                f"({stream.latency * 1000:.0f}ms device latency)")
        state["stream"], state["name"] = stream, name
        state["index"] = _device_index(name)

    def _open(target, name):
        stream = sd.RawOutputStream(
            samplerate=args.rate,
            channels=args.channels,
            dtype="int16",
            device=target,
            blocksize=0,
            latency=current_latency(),
        )
        stream.start()
        # Only worth a line when it is news. Re-opening is how the wanted
        # device is looked for, so it happens on every silence gap and logging
        # each one would bury a real device change in repetition.
        if state["name"] is None:
            log(f"opened {name} ({args.rate}Hz, {args.channels}ch, "
                f"{stream.latency * 1000:.0f}ms device latency)")
        return stream

    # Keep trying at startup rather than exiting.
    #
    # dd-pits is started once and left running for a race day; an audio device
    # that is not ready in the first second is not a reason to be silent for the
    # rest of it.
    for attempt in range(30):
        try:
            open_stream()
            break
        except Exception as ex:
            if attempt == 0:
                log(f"cannot open an output device yet ({type(ex).__name__}: {ex}) — retrying")
            time.sleep(2.0)
    else:
        log("no output device after 60s — giving up")
        return 1

    # Discard buffered audio on demand.
    #
    # Ducking a cue stops dd-pits GENERATING it, but the samples already handed
    # to the device still play: measured at 406ms on Bluetooth, which is heard
    # as the audio carrying on after the Start button. stream.abort() drops
    # them where stream.stop() would drain them, so this is the only way to make
    # a stop actually immediate.
    try:
        signal.signal(signal.SIGUSR1, _on_flush)
    except (AttributeError, ValueError):
        # No SIGUSR1 (Windows) or not the main thread: the buffer simply plays
        # out, which is the behaviour before this existed.
        pass

    # When the current run of silence began; None while audio is playing.
    silent_since = None

    # Underrun bookkeeping. The buffer is grown in response to MEASURED
    # starvation rather than guessed at: a value that is comfortable on the
    # built-in speakers is not necessarily enough for a Bluetooth headset,
    # which adds its own large and variable buffer, and no fixed constant is
    # right for both. Growing only ever costs latency; underrunning costs the
    # call being unintelligible.
    underruns = {"n": 0, "since_grow": 0, "reported": 0}

    # Read what arrives rather than a fixed block: the parent paces the stream,
    # so blocking on a full buffer would ADD latency for no benefit.
    src = sys.stdin.buffer
    try:
        while True:
            chunk = src.read(READ_CHUNK)
            if not chunk:
                break

            # Follow the system default, but only across a real silence gap.
            #
            # Checking is destructive — see DEVICE_RECHECK_SECONDS — so it goes
            # through open_stream(), which closes BEFORE re-initialising
            # PortAudio. Querying with the stream still open invalidates it and
            # the next write fails with "Invalid stream pointer".
            now = time.time()
            if is_silent(chunk):
                if silent_since is None:
                    silent_since = now
                elif (forced is None or state["on_fallback"]) and now - silent_since > DEVICE_RECHECK_SECONDS:
                    # RE-ARMED, not disarmed.
                    #
                    # This used to set silent_since = None, so a quiet spell
                    # re-opened once and then never again until audio played.
                    # A headset unplugged and replugged during that quiet — the
                    # common case, since that is when nobody is talking — left
                    # a stream pointing at a device that no longer existed.
                    # Writes to it keep succeeding, so nothing ever noticed, and
                    # only restarting the agent brought audio back.
                    #
                    # Re-checking every DEVICE_RECHECK_SECONDS of continuous
                    # silence costs a close-and-reopen during silence, which is
                    # inaudible, and catches a reconnect within one interval.
                    silent_since = now

                    # A device that has been starving gets a bigger buffer, applied
                    # here because re-opening mid-speech would drop audio.
                    if underruns["since_grow"] >= UNDERRUNS_BEFORE_GROW:
                        grown = min(current_latency() * 2, MAX_DEVICE_LATENCY)
                        if grown > current_latency():
                            state["latency"] = grown
                            log(f"{underruns['since_grow']} underruns — growing the device "
                                f"buffer to {grown:.2f}s so calls stop breaking up")
                        underruns["since_grow"] = 0

                    # Re-opening is how the wanted device is looked for.
                    #
                    # There is no non-destructive way to ask: PortAudio only
                    # notices new hardware after a re-initialise, and doing that
                    # with a stream open invalidates it ("Invalid stream
                    # pointer"). open_stream() closes first and tries the named
                    # device before the default, so the same call that follows a
                    # moved default also picks the AirPods up when they connect.
                    try:
                        open_stream()
                    except Exception as ex:
                        log(f"could not follow the output device: {ex}")
            else:
                silent_since = None

            # A stream can stop being usable WITHOUT raising.
            #
            # Bluetooth headphones drop and reconnect constantly, and they come
            # back under the same name — so the default-output check sees no
            # change and leaves the old stream in place. Writes to it are
            # accepted and go nowhere: the log looks healthy and the room is
            # silent. Checking that the stream is still active is the only
            # signal that distinguishes the two.
            st = state["stream"]
            if st is not None and not st.active:
                log("output went inactive (device reconnected?) — reopening")
                try:
                    open_stream()
                except Exception as ex:
                    log(f"could not reopen: {ex}")

            if _flush["now"]:
                _flush["now"] = False
                st = state["stream"]
                if st is not None:
                    try:
                        st.abort()      # discards what is queued on the device
                        st.start()      # ready for whatever comes next
                        log("flushed the device buffer — the bus was cleared")
                    except Exception as ex:
                        log(f"could not flush the device: {ex}")

            try:
                # write() reports whether the device ran DRY before this chunk
                # arrived. That was being thrown away, which left the one
                # artefact we cannot otherwise see — a call that rendered
                # perfectly but played back glitched — with no evidence at all.
                #
                # An underrun is the device having nothing to play, so it
                # repeats or drops samples: heard as a robotic, stuttery
                # version of a line that is fine on disk.
                if state["stream"].write(chunk):
                    underruns["n"] += 1
                    underruns["since_grow"] += 1
            except Exception as ex:
                # The device went away mid-stream — a headset unpairing. Try to
                # reopen on whatever is default now rather than dying: the
                # meeting should not need the agent restarted to get its audio
                # back.
                log(f"output failed ({ex}) — reopening")
                try:
                    open_stream()
                except Exception as ex2:
                    log(f"no usable output: {ex2}")
                    break
        # Reported periodically as well as at exit: an operator hearing a
        # broken-up call during a meeting needs the confirmation THEN, not in a
        # summary after the racing has finished.
        if underruns["n"] and underruns["n"] - underruns["reported"] >= 25:
            underruns["reported"] = underruns["n"]
            log(f"{underruns['n']} underruns so far — the output is running dry, "
                f"so calls will sound broken up (buffer {current_latency():.2f}s)")
    except (BrokenPipeError, KeyboardInterrupt):
        pass
    except Exception as ex:
        log(f"playback stopped: {type(ex).__name__}: {ex}")
    finally:
        try:
            state["stream"].stop()
            state["stream"].close()
        except Exception:
            pass

    log("output closed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
