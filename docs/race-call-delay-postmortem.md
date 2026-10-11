# Why race calls were seconds late

Race calls were landing four or more seconds after the lap. Every stage we
measured came back fast. The cause turned out to be 3.2 seconds of audio sitting
in two queues that nobody had ever looked at.

Worth writing down because the search went wrong in an instructive way: the
instruments all said the system was healthy, and they were incapable of showing
the fault.

## The symptom

The operator counted "over four seconds" between a drone crossing the line and
hearing its lap time. Meanwhile the log looked perfect:

```
18:10:15  heat green
18:10:18  Holeshot bob9 3.02      (green + 3.02 ✓)
18:10:34  bob9 15.57              (green + 3.02 + 15.57 ✓)
```

Every figure agreed with every other figure. That agreement was the trap: they
were all read from the same source at the same staleness, so an offset applied
to all of them was invisible in the arithmetic.

## What we measured, and cleared

| Stage | Measured | Verdict |
|---|---|---|
| FPVTrackside HTTP response | 0.6 ms | fine |
| Poll cadence while racing | 100 ms | fine |
| Script (template substitution) | ~0 ms | fine |
| Speech, first audio (streaming) | ~105 ms | fine |
| Device buffer | **883 ms** | **wrong — fixed** |

The 883 ms was real: a stored `audio.latency = 0.2` which CoreAudio rounds up
steeply. Fixing it took three quarters of a second off every call. It was not
four seconds, and the operator still heard the delay.

## The false trail

The hypothesis was that `Race.json` — which the poller reads — was being written
late by FPVTrackside, so everything derived from it was stale. It fitted the
evidence: internally consistent numbers, all shifted.

That led to wiring FPVTrackside's `ExtensionNotifier`, which PUTs each event as
it happens instead of waiting for a file. It worked, and it disproved the
hypothesis:

```
ft events: LAP bob9 lap 1 in 6.35s (pushed at 18:43:21.655, here after 0ms)
```

Zero milliseconds. FPVTrackside knew instantly and told us instantly — so the
poll, at 100 ms, could never have been more than 100 ms behind it either. **The
poll was never the problem.** The push was built for a delay that was not there.

## What it did expose

With the call now made the instant the lap landed, a second call appeared two
seconds behind it:

```
18:38:55  call [TimeTrialEveryLap]: bob9 4.37
18:38:57  call []                 : bob9 4.37
```

Three paths can speak a lap — the poll, the push, and the relay of
FPVTrackside's own spoken log. Deduplicating against the poll had silenced one
and left the relay. Every lap was being announced twice, roughly two seconds
apart, which is a very good impression of "delayed and garbled".

## The actual bug

Only after instrumenting the one stage that had never been measured — the gap
between "audio exists" and "audio is playing" — did the real cause appear.
Reproduced in a test:

```
lap call waited  2,000ms behind  2,000ms of audio
lap call waited 12,000ms behind 12,000ms of audio
```

Two problems, both in dd-pits' own code:

1. **A lap time waited for whatever was playing to finish.** The mixer never cut
   a playing cue — right for its original purpose (calls were being chopped to
   "Pil—" mid-word), wrong when it protects commentary at the expense of a lap
   time. Commentary is now faded out for a race call; a race call still never
   cuts another race call, because losing a lap time is worse than delaying one.

2. **Two chained queues of 32 frames each**, mixer→subscriber and
   player→sidecar. At the 100 ms frame that was 3.2 seconds apiece. They never
   drained: the bus produces at realtime and the device consumes at realtime, so
   one momentary stall filled a queue and that latency became permanent for the
   rest of the session. Bounded to 2 frames (100 ms); a stall now drops a frame
   instead of delaying everything behind it.

Also: every cue waited a full frame to start, not the half-frame average one
would assume, because the mixer picks the next cue at the tick. Halving the
frame to 50 ms halved that floor for every call, cached ones included.

## Why it took so long

Four separate blind spots, all the same shape — **the fault was invisible, not
broken**:

- the audio sidecar reported its achieved latency **only on first open**, so a
  device change hid the 883 ms buffer
- relayed calls were stamped with `time.Now()` on READ, discarding
  FPVTrackside's own timestamp — lateness was unmeasurable by construction
- the new push receiver logged **only problems**, so "working" and "never
  contacted" both looked like silence
- the queues had **no instrumentation at all**

Every stage that MADE audio was measured. The stage that PLAYED it was not. So
"every stage measures fast" was true and useless, and the operator saying "it is
really shit and delayed" was the only reliable signal available.

**The rule: when a reported symptom contradicts the instruments, suspect the
instruments first.** A log that only speaks on failure cannot distinguish
healthy from absent.

## Was the push worth it?

Not for latency. The poll at 100 ms was always fast enough, and the numbers say
so: the push arrives 0–1 ms after FPVTrackside's own timestamp, so polling could
trail it by at most one tick.

It has other merits — `Valid` straight from the timing system, `Seq` making a
dropped event detectable, `PositionSnapshot`, and sector data — and it costs
nothing when absent, since a lap with no push is announced by the poller exactly
as before. But it needs the forked FPVTrackside (`ExtensionMode` is not
upstream), and it did not fix the delay.

Everything that DID fix the delay is in dd-pits and reaches every club on stock
FPVTrackside.
