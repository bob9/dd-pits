<p align="center"><img src="cmd/dd-pits/assets/icon-256.webp" width="96" alt="dd-pits icon"></p>

# dd-pits

**A race-day companion for [FPVTrackside](https://github.com/FPVTrackside) and
[RotorHazard](https://github.com/RotorHazard/RotorHazard).**
dd-pits runs next to your timer (on the FPVTrackside laptop, or on the
RotorHazard Pi as a plugin) and turns its timing data into everything else a
race director wants on the day: a live dashboard
pilots can open on their phones, OBS overlays, spoken race calls, AI
commentary, and lap times in pilots' goggles via an ELRS backpack.

It runs entirely on the venue laptop and needs no internet connection or
account. Everything is configured in a browser and stored in one JSON file
beside the binary.

**[Website](https://dd-pits.threedigitalllamas.com.au)** ·
**[Live demo](https://demo.dd-pits.threedigitalllamas.com.au)** ·
**[Download](#download)** ·
**[Set up with FPVTrackside](#set-up-with-fpvtrackside)** ·
**[RotorHazard plugin](#rotorhazard)** ·
**[Brackets](#brackets)** ·
[ELRS Netpack](https://github.com/bob9/elrs-netpack) ·
made by **[bob9](https://github.com/bob9)**

[![Buy Me a Coffee](https://img.shields.io/badge/Buy%20Me%20a%20Coffee-support%20dd--pits-FFDD00?logo=buymeacoffee&logoColor=black)](https://buymeacoffee.com/bobnine)

![The live dashboard during a heat](docs/screenshots/dashboard.webp)

## Screenshots

| | |
| --- | --- |
| ![Live TV view](docs/screenshots/live.webp) **TV view** (`/live`): the current race, full-screen. | ![OBS overlay on a stream](docs/screenshots/overlay.webp) **OBS overlay** (`/overlay`): a broadcast panel for your stream. |
| ![Pilot analytics page](docs/screenshots/pilot.webp) **Pilot pages:** stats, lap chart and every lap of the meeting. | ![Race history](docs/screenshots/races.webp) **Race history:** every heat with its lap times. |
| ![Commentary setup](docs/screenshots/booth.webp) **Commentary booth:** choose a writer, a voice and a style. | ![Goggle pilots setup](docs/screenshots/pilots.webp) **Goggle pilots:** one bind phrase per pilot. |
| ![Race calls setup](docs/screenshots/calls.webp) **Race calls:** decide who speaks the lap times. | ![First-run screen](docs/screenshots/waiting.webp) **First run:** the board lights up when FPVTrackside does. |

### OBS overlays

22 broadcast overlays, all transparent and all following the meeting on their own.
Open **`/overlays`** in dd-pits to see them live. Each has a **Customise** panel
(title, colours, which heat, how many pilots and so on) with a live preview, and
**Add to OBS** puts it straight into your current OBS scene over obs-websocket.
You can also copy the URL into an OBS Browser source at 1920×1080.
[Try them in the live demo →](https://demo.dd-pits.threedigitalllamas.com.au/overlays)

**Live timing**

| | | |
| --- | --- | --- |
| ![Scorebug](docs/screenshots/overlays/scorebug.webp)<br>**Scorebug** `/overlay/scorebug` | ![Top bar](docs/screenshots/overlays/topbar.webp)<br>**Top bar** `/overlay/topbar` | ![Corner](docs/screenshots/overlays/corner.webp)<br>**Corner** `/overlay/corner` |
| ![Race grid](docs/screenshots/overlays/race.webp)<br>**Race grid** `/overlay/race` | ![Pilot cam tag](docs/screenshots/overlays/node-2.webp)<br>**Pilot cam tag** `/overlay/node/2` | ![Racing now strip](docs/screenshots/overlays/strip.webp)<br>**Racing now strip** `/overlay/strip` |
| ![Lap chart](docs/screenshots/overlays/lapchart.webp)<br>**Lap chart** `/overlay/lapchart` | ![Fastest lap](docs/screenshots/overlays/fastest.webp)<br>**Fastest lap** `/overlay/fastest` | ![Race panel](docs/screenshots/overlays/panel.webp)<br>**Race panel** `/overlay/panel` |
| ![Lower third](docs/screenshots/overlays/lowerthird.webp)<br>**Lower third** `/overlay/lowerthird` |  |  |

**Between heats**

| | | |
| --- | --- | --- |
| ![Up next strip](docs/screenshots/overlays/strip-next.webp)<br>**Up next strip** `/overlay/strip/next` | ![Lineup](docs/screenshots/overlays/lineup-next.webp)<br>**Lineup** `/overlay/lineup/next` | ![Lineup, racing](docs/screenshots/overlays/lineup.webp)<br>**Lineup, racing** `/overlay/lineup` |
| ![Up Now](docs/screenshots/overlays/upnow-next.webp)<br>**Up Now** `/overlay/upnow/next` | ![Up Now banner](docs/screenshots/overlays/banner-next.webp)<br>**Up Now banner** `/overlay/banner/next` |  |

**Standings**

| | | |
| --- | --- | --- |
| ![Spotlight](docs/screenshots/overlays/spotlight.webp)<br>**Spotlight** `/overlay/spotlight?p=A&p=B` | ![Leaderboard](docs/screenshots/overlays/leaderboard.webp)<br>**Leaderboard** `/overlay/leaderboard` | ![Laps rail](docs/screenshots/overlays/laps.webp)<br>**Laps rail** `/overlay/laps` |
| ![Standings](docs/screenshots/overlays/standings.webp)<br>**Standings** `/overlay/standings` | ![Podium](docs/screenshots/overlays/podium.webp)<br>**Podium** `/overlay/podium` | ![Results card](docs/screenshots/overlays/results.webp)<br>**Results card** `/overlay/results?title=…&r=Name|time` |
| ![Notice](docs/screenshots/overlays/notice.webp)<br>**Notice** `/overlay/notice?text=…` |  |  |

<p align="center"><img src="docs/screenshots/mobile.webp" width="300" alt="The dashboard on a phone"><br><em>Every pilot can follow along on their phone.</em></p>

## Features

### Dashboard and displays

- **Live dashboard** (`/`): the current heat, who's next, standings, best
  consecutive laps and time trials. Anyone on the venue wifi can open it on
  their phone, and it updates live without refreshing.
- **Pilot pages** (`/dronedash/pilot?name=…`): per-pilot analytics, with stat
  cards that jump to the heat that set them, a lap chart, and lap and race
  tables. Everything is rendered on the laptop, so it works offline.
- **Race history** (`/dronedash/races`): every heat with its lap times.
- **TV view** (`/live`): the current race full-screen, for a screen at the
  pits.
- **OBS overlay** (`/overlay`): add it as a Browser Source.
- **Practice rounds** show on the board but don't count towards standings.
- **Slow displays:** add `?lite=1` to any page for an old TV or tablet that
  struggles.

### Race calls

- **One voice for every call:** laps, holeshots, results and "next up" can be
  spoken by dd-pits instead of FPVTrackside's own speech.
- **Your club's wording:** calls are read from FPVTrackside's own
  `Sounds.xml`, so the phrasing and on/off switches you set there carry over.
  dd-pits finds FPVTrackside's files itself, and an audit flags settings that
  disagree.
- **One switch decides who speaks.** dd-pits sets FPVTrackside's speech volume
  to match, so you never hear both or neither. If no voice is set up,
  FPVTrackside keeps speaking and dd-pits stays out of the audio.
- **Fast calls:** dd-pits listens for FPVTrackside's live events as well as
  polling it. Voice packs of pre-generated clips make common calls play
  instantly.
- **Race director controls** (`/calls`): Hurry Up, Announce Race and the
  other calls FPVTrackside binds to keyboard shortcuts. Trigger them from the
  laptop or any phone at the field.
- **Pronunciations:** tell the voice how to say a tricky callsign without
  changing how it's spelled anywhere else.
- When the operator moves to the next race, anything still waiting to be said
  about the last one is dropped.

### Commentary booth

- **AI commentary:** play-by-play and colour written from the live timing
  data. Commentary and race calls share one audio bus, so a lap call always
  cuts in instead of waiting behind colour.
- **Choose where it runs:**
  - Writer: a local model on Apple Silicon (MLX), Google Gemini, or Qwen.
  - Voice: a local cloned voice (Qwen3-TTS on MLX), or Qwen3-TTS over the
    network.
  - Mix and match. `/setup/models` checks the machine and installs the local
    models for you.
- **Your own commentator:** clone a voice from a recording you upload, or
  record one straight in the browser. Qwen can also design a voice from a
  description.
- **Style:** personality, humour, energy, language and accent. Commentary can
  be solo or a two-person duo, and you choose which phases (before, during and
  after the race) and which moments it talks about.
- **Hearing it:** play it through the laptop's speakers, or add `/booth.wav` to
  OBS as a Media Source. With the OBS websocket connected, dd-pits delays the
  video automatically so the commentary stays in sync, and can add the audio to
  every scene.
- **Cost tracking:** the `/booth` page shows the session's AI spend and recent
  on-air lines.
- **Model host mode** (`-model-host` / `-model-url`): a Windows race PC can
  borrow a Mac's GPU over the LAN.

### Goggles (ELRS backpack)

- **On-screen info:** lap times and race info appear in pilots' goggles. Each
  pilot needs only a bind phrase; dd-pits derives the goggle ID from it.
- **DVR names:** recordings are named after the heat
  (`hdz_0042-WinterCup-Mains-A-R5-H1.mp4`).
- **Clock sync:** the goggle clock is set correctly.
- **Any band, including Lowband:** set a pilot to Raceband or L1–L8 from the
  desk; their goggles switch band and tune.
- **Channel from the goggles:** a channel picked on the goggles is sent on to
  the drone's VTX and the timer.

  DVR names, Lowband and channel-from-goggles need
  [race-day HDZero firmware](#race-day-firmware-for-hdzero-goggles).
- **Configurable screen:** the OSD layout is set at `/setup/osd`, with an
  optional custom layout per pilot.
- **Race channels:** on Goggle control (`/goggles`), one button puts every
  pilot in the race on the line, or the race up next, on their race channel.
  Switch on *Race channels as each race is cued* and dd-pits does it by itself
  the moment a race is cued.
- **Goggle control** (`/goggles`): change a pilot's video channel from the
  desk, or send a message to some or all pilots.
- **Connection:** an [ELRS Netpack](#works-with-elrs-netpack) on the venue
  network (it can confirm delivery), a USB backpack, or netpack with USB
  fallback.

### After the race

- **Watch links:** paste a YouTube stream address and place one heat on the
  timeline. Every heat then gets a ▶ Watch link, and dd-pits builds a chapter
  list to paste into the video description.
- **Commentary after the fact:** generate commentary for finished heats, and
  the Watch player plays it over the muted video, in sync as you scrub.

### Running it

- **Browser setup:** everything is configured at `/setup`, with a guided
  first run and help at `/setup/help`. Settings are stored in one JSON file you
  can copy to another laptop.
- **Debug page** (`/setup/debug`): the log in the browser, with API keys
  removed, plus a downloadable report to send when something goes wrong.
- **JSON API** (`/docs`): meeting, race, round, leaderboard, pilot and channel
  data as JSON, with an OpenAPI spec and a live event stream, for building
  your own displays.

## Works with ELRS Netpack

[**ELRS Netpack**](https://github.com/bob9/elrs-netpack) is open-source
firmware that turns an Ethernet-equipped ESP32 board into an ExpressLRS timer
backpack you reach over the network instead of a USB cable. dd-pits talks to it
over TCP (port `8080`, found at `elrs-netpack.local`), and it relays goggle
messages to every pilot at the field. That covers lap times on the OSD, DVR
names, clock sync and channel changes.

- **One cable:** on a PoE board (Waveshare ESP32-S3-ETH PoE, Olimex
  ESP32-POE-ISO) a single Ethernet cable powers the netpack and connects it.
  Put it anywhere on the venue network, not tethered to the timing laptop.
- **Delivery confirmation:** the netpack reports back whether each message
  reached the goggles, so dd-pits knows a pilot actually got it.
- **Over-the-air updates:** browse to `http://elrs-netpack.local/` and drop in
  a new firmware file. No USB needed after the first flash.
- **Goggle test console** (`/test`): send an OSD message, change a channel,
  set the clock or name a DVR recording from any browser. Useful for checking
  a pilot's goggles before the meeting starts.

| Firmware console | Goggle test console |
| --- | --- |
| ![ELRS Netpack firmware update page](docs/screenshots/netpack-update.webp) | ![ELRS Netpack goggle test console](docs/screenshots/netpack-test.webp) |

Set it up in dd-pits at `/setup/backpack`: choose **Network (netpack)** and
enter its name or address. ELRS Netpack was originally created by
[i-am-grub](https://github.com/i-am-grub/elrs-netpack), and is forked and
extended by [bob9](https://github.com/bob9/elrs-netpack). It is not affiliated
with the ExpressLRS project.

## Race-day firmware for HDZero goggles

Stock HDZero goggles show dd-pits' lap times on the OSD with any ExpressLRS
backpack. Three race-day features need firmware from
[bob9's HDZero goggle fork](https://github.com/bob9/hdzero-goggle) and
[bob9's Backpack fork](https://github.com/bob9/Backpack), flashed as a pair
(one goggle file, one backpack file):

- **Lowband from the timer.** On stock firmware a Lowband channel (L1–L8) sent
  by the timer never arrives: the goggles only switch band from their own
  menu. With the fork, setting a pilot to L1–L8 from dd-pits' **Goggle
  control** (`/goggles`) switches the goggles into Lowband and tunes them, and
  a Raceband channel switches them back.
- **Change channel from the goggles.** Pick a channel on the goggles and the
  backpack broadcasts it: the drone's VTX, the radio's VTX Administrator and
  the timer all follow. *Auto Send VTX* on the goggles' ELRS page sends on
  every change; a dial long-press sends (or re-sends) on demand, and a
  `VTX SENT` pill confirms it on screen. The dial also reaches every band,
  Lowband included. Leave Auto Send VTX off when
  the timer assigns channels, or spectating another channel retunes your own
  quad.
- **DVR recordings named after the heat.** dd-pits sends each pilot a label
  for their next recording as their heat is staged, and the goggles append it
  to the usual file name: `hdz_0042-WinterCup-Mains-A-R5-H1.mp4`. Labels can be
  up to 43 characters, so the meeting name fits.

The same builds set the goggle clock from the timer, so recordings carry the
right date.

| Firmware | Release | What it adds |
| --- | --- | --- |
| HDZero goggles | [9.5.45](https://github.com/bob9/hdzero-goggle/releases/tag/v9.5.45) | Stock HDZero plus the three features above, the all-band dial and clock sync; for Goggle, Goggle 2 and BoxPro |
| Goggle backpack | [AUFPV race build](https://github.com/bob9/Backpack/releases/tag/v2026.07.17-aufpv-race) | Forwards the DVR name and clock, and broadcasts goggle channel changes |

The goggle changes are submitted upstream as
[hd-zero/hdzero-goggle#613](https://github.com/hd-zero/hdzero-goggle/pull/613)
and [#614](https://github.com/hd-zero/hdzero-goggle/pull/614). Neither fork is
affiliated with HDZero or the ExpressLRS project.

## Download

Every change to `main` is built and released automatically. These links always
fetch the latest build:

| Computer | Download |
| --- | --- |
| Mac, Apple Silicon (M1 or later) | [dd-pits-darwin-arm64](https://github.com/bob9/dd-pits/releases/latest/download/dd-pits-darwin-arm64) |
| Mac, Intel | [dd-pits-darwin-amd64](https://github.com/bob9/dd-pits/releases/latest/download/dd-pits-darwin-amd64) |
| Windows | [dd-pits-windows-amd64.exe](https://github.com/bob9/dd-pits/releases/latest/download/dd-pits-windows-amd64.exe) |
| Linux PC | [dd-pits-linux-amd64](https://github.com/bob9/dd-pits/releases/latest/download/dd-pits-linux-amd64) |
| Raspberry Pi, 64-bit OS | [dd-pits-linux-arm64](https://github.com/bob9/dd-pits/releases/latest/download/dd-pits-linux-arm64) |
| Raspberry Pi, 32-bit OS | [dd-pits-linux-arm](https://github.com/bob9/dd-pits/releases/latest/download/dd-pits-linux-arm) |
| **RotorHazard timer** | [dd-pits-rotorhazard-plugin.zip](https://github.com/bob9/dd-pits/releases/latest/download/dd-pits-rotorhazard-plugin.zip). See [RotorHazard](#rotorhazard) |

**Mac:** run `chmod +x dd-pits-darwin-*` and
`xattr -d com.apple.quarantine dd-pits-darwin-*` once in Terminal.
**Windows:** if SmartScreen warns, choose More info → Run anyway.

## Set up with FPVTrackside

Run dd-pits on the **same laptop** as FPVTrackside. It takes about five minutes.

**1. Turn on FPVTrackside's web server.** This is the one setting dd-pits needs,
and it is off by default. In FPVTrackside open **Menu → Application Settings**.
Under **Web**, tick **Auto start HTTP Server**, then restart FPVTrackside. To
check it worked, open <http://localhost:8080> in a browser and FPVTrackside's
page should appear.

**2. Start dd-pits.** [Download](#download) it, put it anywhere, and run it.
With no arguments it serves on port `8088` and opens its setup guide in your
browser on first run.

**3. Open your event in FPVTrackside.** The dashboard at
<http://localhost:8088> lights up with the meeting. Give pilots
`http://<this-laptop's-address>:8088` over the venue wifi; the setup overview
shows the exact address.

That's all you need for the dashboard, TV view, overlays and API. The next two
steps are optional.

**4. Faster lap calls (optional).** Let FPVTrackside push each lap the moment
it happens, instead of dd-pits polling for it. In **Menu → Application
Settings**, under **Gate / LED POST notifications**, set:

| Setting | Value |
| --- | --- |
| Notifier enabled / mode | `LegacyMode`, or `ExtensionMode` if your version has it |
| NotificationURL | `http://localhost:8088/fpvtrackside/events` |

Restart FPVTrackside. If something already uses NotificationURL (LED gates,
strobes), start dd-pits with `DD_PITS_FT_FORWARD=<their url>` and it passes
every event on to them too.

**5. dd-pits speaks the race calls (optional).** At **Setup → Race calls**
(`/setup/calls`), choose **dd-pits**. It reads your wording from FPVTrackside's
`Sounds.xml` (found automatically; pick the profile at **Setup → FPVTrackside**
if you have several). It also turns FPVTrackside's own speech down so you don't
hear both. Restart FPVTrackside when the page says to.

**6. Edit the meeting from dd-pits (optional).** With the
[FPVTrackside alpha](https://alpha.fpvtrackside.com/) (the first version with
FPVTrackside's REST API), **Race director → Meeting editor** (`/meeting`) changes the
meeting in FPVTrackside itself: the event's name, laps, race length and
channels; rounds (add, rename, delete); races (add, delete, reset, select, laps
to finish); each race's line-up and channels; and the event's pilots.
FPVTrackside has no race names, so a race's name is kept by dd-pits and shown
on dd-pits's pages and overlays. From another machine, give dd-pits
FPVTrackside's API key as `FPV_API_TOKEN`.

> **Tip:** [bob9's FPVTrackside fork](https://github.com/bob9/FPVTracksideCore)
> adds a `/current` endpoint, which lets the dashboard show the next heat the
> moment it is cued rather than when it starts. dd-pits works fine with stock
> FPVTrackside without it.

The in-app help at `/setup/help` covers the rest of race-day setup.

## RotorHazard

dd-pits also runs as a **RotorHazard plugin**, so a club timing on RotorHazard
gets the same features: the phone dashboard with the current race, up-next
heats and leaderboard, the TV view, all the OBS overlays, race history, pilot
pages, spoken race calls, the commentary booth, and goggle OSD over ELRS.

The plugin runs inside RotorHazard. It shares RotorHazard's pilots, heats,
classes, laps and results with dd-pits, and downloads, starts and supervises
dd-pits on the timer itself. Nothing else needs to run on a laptop.

**Works with RotorHazard 4.0 and later**, tested on 4.0.1, 4.1.1, 4.2.1, 4.3.1
and 4.4.0. RotorHazard 3.x has no plugin system, so it is not supported.

**1. Install the plugin.** Download
[dd-pits-rotorhazard-plugin.zip](https://github.com/bob9/dd-pits/releases/latest/download/dd-pits-rotorhazard-plugin.zip).

- **RotorHazard 4.3 or later:** go to **Settings → Plugins**, upload the zip, and
  restart RotorHazard.
- **RotorHazard 4.0 to 4.2:** unzip it and copy the `dd_pits` folder into
  `RotorHazard/src/server/plugins/` on the timer, then restart RotorHazard.

**2. Start it once with internet.** On its first start the plugin downloads the
right dd-pits for the timer (64-bit or 32-bit Pi, Linux PC, Mac or Windows) and
checks it against the release checksums. After that, no internet is needed.
To set it up offline, put a dd-pits binary on the timer and enter its path under
**Use my own dd-pits program**.

**3. Open the dashboard.** Pilots open `http://<timer>:8088` on the venue
wifi. RotorHazard's **Settings → dd-pits** panel links to the dashboard, the OBS
overlays and dd-pits' own settings. It also has Start/restart, Stop and Update
buttons, plus the port setting.

The **Set up dd-pits** link is behind RotorHazard's admin login. It opens
dd-pits' settings from your laptop or phone with a PIN the plugin generates,
because the timer has no screen of its own.

| RotorHazard | dd-pits |
| --- | --- |
| The selected heat | Current race: cued while staging, live while racing |
| The next heats after it | Up next, with lineups and channels |
| Saved races and their results | Race history and positions |
| Classes and rounds | "Open Round 2 — Race 3" labels |
| Fastest-consecutive formats | Time-trial scoring |
| Seat frequencies and colours | Channel labels and colours |

**Race calls.** FPVTrackside lets you edit the wording of each call; RotorHazard
has no equivalent, so on RotorHazard dd-pits uses FPVTrackside's standard calls
("Vortex lap 2 in first"). If dd-pits speaks the calls, turn off RotorHazard's
own voice callouts so they aren't spoken twice. For goggle OSD, use either
dd-pits or RotorHazard's own ELRS plugin, not both.

The plugin also adds **dd-pits** to RotorHazard's own menu at the top of every
page, and brackets and OBS overlays to its ⋮ menu. You can turn this off with
**Show dd-pits in RotorHazard's menu**.

**A timer on the internet.** If RotorHazard is published through a reverse
proxy, serve dd-pits under a path on the same site and tell the plugin that path
(**Public dashboard address** `/pits`):

```nginx
location = /pits { return 301 /pits/; }
location /pits/ {
    proxy_pass http://127.0.0.1:8088;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Prefix /pits;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_buffering off;          # live updates and commentary audio
    proxy_read_timeout 1h;
}
```

dd-pits rewrites its links for requests that arrive with
`X-Forwarded-Prefix`, so the same dd-pits still works directly on its port at
a venue. Through a proxy, changing dd-pits' settings always needs the setup
PIN. The plugin's **Set up dd-pits** link supplies it to RotorHazard admins.

Without the plugin, any dd-pits can still read a RotorHazard timer that has the
plugin installed: `dd-pits -rotorhazard http://<timer>:5000`.

## Marshalling

**Race director → Marshal** (`/marshal`) fixes a race's laps. With a
**RotorHazard** timer (on its own, or as FPVTrackside's timer) it works from
each pilot's signal trace, the way RotorHazard's own Marshal page does, and
goes further:

- **Every pilot on one time axis.** Each pilot's signal with the red enter and
  orange exit lines; counted passes shaded, lap times on the trace. Scroll to
  zoom, drag to pan. A bleed from another channel lines up with that pilot's
  pass right above it.
- **Drag the lines** and the laps recalculate as you go, by RotorHazard's own
  rule (a pass starts above enter, ends below exit, and is stamped at its peak),
  so what you save is what RotorHazard would have counted.
- **Auto-detect** finds the lines from the trace itself: it measures the
  background and the passes, tries lines across the gap and keeps the ones that
  give steady lap times, no impossible laps, the race's lap count and the most
  room either side, then says how sure it is and why.
- **Problems found for you:** a lap far too short (a double count, or a stray
  signal), one far too long (a missed pass, with the weak peak where it
  probably was and an *Add lap here* button), a pass only just over the line,
  another pilot's signal bleeding into this channel, too few laps for the race.
- **Click** a pass to remove or restore it, **shift-click** the trace to add a
  missed lap. *Save laps* saves through RotorHazard's own marshal save, so
  results, brackets, the dashboard and FPVTrackside all follow. *Use these lines
  from now on* sets the node's enter/exit for the races to come.

**On RotorHazard** it's in RotorHazard's menu under *dd-pits → Marshal*.
**Beside FPVTrackside with a RotorHazard timer**, dd-pits finds the timer's
address in FPVTrackside's own settings (or set it at *Settings → FPVTrackside*)
and reads the traces through the dd-pits plugin on that RotorHazard (give dd-pits
the key shown in RotorHazard's dd-pits settings), or through the FPVTrackside
connector 1.2.0 or later. With other timers, the Marshal page lists each pilot's
crossings to remove or add by hand.

## Brackets

dd-pits runs **stages**: competitions made of several races, where each result
decides who flies next. They work on RotorHazard and on FPVTrackside. The draw
and progression come from the same engine as the Stages in
[bob9's FPVTrackside](https://github.com/bob9/FPVTracksideCore) and the AUFPV
platform, so a stage gives the same races in all three.

| Format | What it is |
| --- | --- |
| Double elimination | Winners and losers brackets; a pilot is out after two losses. Ends in a grand final (with reset) or chase the ace. Single elimination too |
| Double elimination: top 4 advance | The MultiGP qualifier sheet. Ends in a final that ranks the top four, who qualify onward |
| Bracket progression | A qualifier per bracket (A, B, C…). The lowest bracket races first and its top finishers move up |
| Ladder | The lowest seeds race first; the top finishers climb into the next race against higher seeds |
| Points brackets | Pilots split into brackets by seed, several rounds each, ranked by a points table (drop-worst optional) |

**Set one up** at **Brackets → Create bracket** (`/stages/new`). Pick the format and
pilots, seeded from the meeting's results (best consecutive laps). Drag to
reorder, then set the options: pilots per race, how many advance, finals, and
so on.

**Running it.** Each race is sent to the timer **as soon as its line-up is
known**: round 1 straight away, later races as results come in.

- **On RotorHazard,** each race becomes a heat in the bracket's own race
  class, one heat group per round, with the pilots seated in seed order.
- **On FPVTrackside,** races are created through FPVTrackside's REST API, one
  round per bracket round, and dd-pits starts them and reads the results back.
  For now this needs the [FPVTrackside alpha](https://alpha.fpvtrackside.com/),
  the first version with the REST API; on an older one you add the races by
  hand and enter the results on the stage page.

When a race ends, its finishing order is read from the timing and the bracket
moves on: losers drop into the losers bracket, finalists are seated, and unused
chase-the-ace races are marked "not needed".

**Results** can also be entered or corrected by hand on the stage page. A
hand-entered result is locked against timing. A result can be cleared as long as
no later race that depends on it has been flown.

**Watching.** Anyone on the venue wifi can follow the bracket at `/stages`.
There is also a JSON feed at `/stages/<id>.json`.

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

### Developing without a timing system

`cmd/fpvtrackside-sim` serves a simulated FPVTrackside meeting over the same
API dd-pits reads. Heats are staged, raced and finished back to back in real
time, so every page has live data to show:

```sh
go run ./cmd/fpvtrackside-sim -addr 127.0.0.1:8080 &
go run ./cmd/dd-pits -fpvtrackside http://127.0.0.1:8080
```

The [live demo](https://demo.dd-pits.threedigitalllamas.com.au) runs exactly
this.

## Project layout

```
cmd/dd-pits/         the application: poller, local web server, setup pages,
                     race calls, commentary booth, ELRS controller
internal/dronedash/  FPVTrackside and RotorHazard clients, snapshot model
internal/stages/     bracket engine: elimination, qualifier, progression,
                     ladder, points brackets (shared with the AUFPV platform)
rotorhazard/dd_pits/ the RotorHazard plugin (Python)
internal/ddapi/      the JSON read API served at /docs
internal/booth/      commentary prompt building and style
internal/elrs/       ELRS backpack protocol (serial + netpack)
internal/tts/        speech engines (local MLX sidecar, Gemini, Qwen)
internal/gemini/     Gemini client (AI Studio key or Vertex AI)
internal/qwenchat/   Qwen chat client (commentary writer)
internal/qwentts/    Qwen3-TTS client
internal/localchat/  local MLX chat model sidecar
cmd/fpvtrackside-sim/ simulated FPVTrackside meeting for development and demos
site/                the promo website (dd-pits.threedigitalllamas.com.au)
docs/                design notes and screenshots
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

## Author

dd-pits is made by **[bob9](https://github.com/bob9)** (Glenn Pringle), a race
director who builds the tools his own race days need. Also by bob9:
[ELRS Netpack](https://github.com/bob9/elrs-netpack).

## License

[MIT](LICENSE)
