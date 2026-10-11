# dd-pits

**A race-day companion for [FPVTrackside](https://github.com/FPVTrackside).**
dd-pits runs on the timing laptop next to FPVTrackside and turns its timing
data into everything else a race director wants on the day: a live dashboard
pilots can open on their phones, OBS overlays, spoken race calls, AI
commentary, and lap times in pilots' goggles via an ELRS backpack.

It runs entirely on the venue laptop and needs no internet connection or
account. Everything is configured in a browser and stored in one JSON file
beside the binary.

[![Buy Me a Coffee](https://img.shields.io/badge/Buy%20Me%20a%20Coffee-support%20dd--pits-FFDD00?logo=buymeacoffee&logoColor=black)](https://buymeacoffee.com/bobnine)

## Features

- **Live dashboard** (`/`): current heat, standings, best consecutive laps,
  time trials, and per-pilot analytics. Anyone on the venue wifi can open it.
- **Race history** (`/dronedash/races`): every heat with its lap times.
- **TV and stream views**: full-screen current race (`/live`) and an OBS
  browser-source overlay (`/overlay`).
- **Race calls**: lap times, holeshots and results spoken in one consistent
  voice. You can leave FPVTrackside speaking its own calls, and dd-pits stays
  out of the audio.
- **Commentary booth**: AI-written play-by-play and colour, mixed with the race
  calls on a single audio bus. Serve it to OBS as `/booth.wav` or play it
  through the laptop's speakers. Optional OBS websocket integration delays the
  video so the commentary lines up with it.
  - **Writer**: a local model on Apple Silicon (MLX), Google Gemini, or Qwen.
  - **Voice**: a local cloned voice (Qwen3-TTS on MLX), or Qwen3-TTS over the
    network.
  - **Model host mode** (`-model-host` / `-model-url`) lets a Windows race PC
    borrow a Mac's GPU over the LAN.
- **ELRS backpack OSD**: lap times in pilots' goggles, DVR recordings named
  after the heat, goggle clock sync, channel changes from the desk, and
  announcements to pilots. Works over a netpack (TCP) or a USB backpack.
- **YouTube chapters**: anchor one heat in a recorded stream and every heat
  gets a Watch link, plus a chapter list to paste into the video description.
  Finished heats can have commentary generated after the fact.
- **JSON API** (`/docs`): build your own displays.

## Quick start

1. Download the binary for your platform from
   [Releases](../../releases), or build it yourself (see below).
2. Start FPVTrackside and open your event.
3. Run `dd-pits`. With no arguments it serves on port `8088` and opens the
   setup guide in your browser on first run.
4. Open `http://localhost:8088/` to check the dashboard can see your event,
   then work through `http://localhost:8088/setup`.

The in-app help at `/setup/help` covers race-day setup in detail.

### Settings and flags

Settings are saved to `dd-pits-local.json` beside the binary. Copy that file to
another laptop to move the whole setup. Every setting can also be given as a
command-line flag or `DD_PITS_*` environment variable, and a flag always wins
over the file. Run `dd-pits -h` for the full list. The ones you are most likely
to need:

| Flag | Default | Purpose |
| --- | --- | --- |
| `-fpvtrackside` | `http://localhost:8080` | Where FPVTrackside's web server is |
| `-serve-addr` | `:8088` | Address for dd-pits' own pages |
| `-setup-pin` | (none) | Allow changing settings from another device with `?pin=…` |
| `-race-audio` | off | Speak FPVTrackside's race calls from dd-pits |
| `-booth` | off | Run the AI commentary booth |
| `-elrs` | off | Enable the ELRS backpack |
| `-audio-out` | off | Play the audio bus on this machine's speakers |

The settings pages accept changes only from the laptop itself. The dashboard is
open to the whole venue network, so this stops a phone in the pits from
changing someone's goggle channel. Use `-setup-pin` if you need to configure
from another device.

### Local AI models (optional)

Local voice and local commentary need **Apple Silicon** and a Python
environment with `mlx-audio` (voice) and/or `mlx-lm` (writer). The
`/setup/models` page checks what is installed and can install the models for
you. Playing through the laptop's speakers needs `sounddevice`. The Python
sidecar scripts are embedded in the binary and written out beside it at
startup.

On other hardware, use a Gemini or Qwen API key for the writer and voice, or
point `-model-url` at a Mac running `dd-pits -model-host :9099`.

## Building

You need Go 1.25 or later.

```sh
make build          # ./bin/dd-pits for this machine
make test           # go vet + go test
make release        # cross-compile for macOS, Windows and Linux into ./dist
```

On macOS, in-process audio uses CGo (`ebitengine/oto`). Cross-compiled builds
fall back to the Python audio sidecar.

## Project layout

```
cmd/dd-pits/         the application: poller, local web server, setup pages,
                     race calls, commentary booth, ELRS controller
internal/dronedash/  FPVTrackside client and snapshot model
internal/ddapi/      the JSON read API served at /docs
internal/booth/      commentary prompt building and style
internal/elrs/       ELRS backpack protocol (serial + netpack)
internal/tts/        speech engines (local MLX sidecar, Gemini, Qwen)
internal/gemini/     Gemini client (AI Studio key or Vertex AI)
internal/qwenchat/   Qwen chat client (commentary writer)
internal/qwentts/    Qwen3-TTS client
internal/localchat/  local MLX chat model sidecar
docs/                design notes
```

## Contributing

Issues and pull requests are welcome. Run `make test` before sending a PR. If
you change anything that touches race-day timing (the poll loop, race calls or
the audio bus), say how you tested it against a real or recorded FPVTrackside
event.

## Support

dd-pits is free and built in spare time between race days. If it makes your
club's events run smoother, you can help keep it going at
**[buymeacoffee.com/bobnine](https://buymeacoffee.com/bobnine)** ☕

## License

[MIT](LICENSE)
