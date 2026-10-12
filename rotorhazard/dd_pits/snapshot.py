"""Translate RotorHazard's race data into the meeting snapshot dd-pits reads.

dd-pits was written against FPVTrackside's HTTP API, and everything it does —
the dashboard, the OBS overlays, the race calls, the commentary, the goggle
OSD — works from one in-memory "snapshot" of the meeting in that shape. So
rather than teach dd-pits a second data model, this module builds the same
snapshot out of RotorHazard's database and live race:

    RotorHazard                      dd-pits snapshot (FPVTrackside shape)
    -----------                      -------------------------------------
    pilot                            Pilot
    heat run in a round              Race      ("rh-h<heat>-r<round>")
    race class + round number        Round
    seat frequency                   Channel
    saved race, the live race,       Races in running order: run, running,
      the next heats to run            then pending
    lap crossings                    Detections + Laps
    race results                     Results

Times are sent as UTC RFC3339, so the timer's timezone never matters.
"""

import hashlib
import json
from datetime import datetime, timezone
from time import monotonic

# RotorHazard's RaceStatus values (RHRace.RaceStatus).
READY, RACING, DONE, STAGING = 0, 1, 2, 3

# Win conditions and start behaviours from RotorHazard's race formats.
WIN_FIRST_TO_X = 2
WIN_FASTEST_CONSECUTIVE = 4
START_FIRST_LAP = 1

# How many not-yet-run heats to list after the current one, for "Up next" and
# the Upcoming page. Generous, so pilots can see when they fly all day; capped
# so a database full of old heats can't make every poll slow.
PENDING_HEATS = 60

# Standard 5.8GHz channels, for naming a frequency the current frequency set
# no longer uses (an old race run on a different set).
_BANDS = {
    "R": [5658, 5695, 5732, 5769, 5806, 5843, 5880, 5917],
    "F": [5740, 5760, 5780, 5800, 5820, 5840, 5860, 5880],
    "E": [5705, 5685, 5665, 5645, 5885, 5905, 5925, 5945],
    "A": [5865, 5845, 5825, 5805, 5785, 5765, 5745, 5725],
    "B": [5733, 5752, 5771, 5790, 5809, 5828, 5847, 5866],
    "L": [5362, 5399, 5436, 5473, 5510, 5547, 5584, 5621],
}
_BAND_NAMES = {"R": "Raceband", "F": "Fatshark", "E": "Boscam E", "A": "Boscam A",
               "B": "Boscam B", "L": "Lowband"}

_DEFAULT_COLORS = ["#0022ff", "#ff5500", "#00ff22", "#ff0055",
                   "#ddff00", "#9900ff", "#00ffff", "#888888"]


def utc(epoch_ms):
    """An epoch-milliseconds instant as dd-pits wants it."""
    if not epoch_ms:
        return ""
    t = datetime.fromtimestamp(epoch_ms / 1000.0, timezone.utc)
    return t.strftime("%Y-%m-%dT%H:%M:%S.") + "%03dZ" % (t.microsecond // 1000)


def local_str_to_epoch_ms(s):
    """RotorHazard stores a saved race's start as the timer's local wall clock."""
    if not s:
        return 0
    for fmt in ("%Y-%m-%d %H:%M:%S.%f", "%Y-%m-%d %H:%M:%S"):
        try:
            return int(datetime.strptime(s, fmt).timestamp() * 1000)
        except ValueError:
            pass
    return 0


def race_id(heat_id, round_num):
    return "rh-h%s-r%s" % (heat_id, round_num)


def pilot_id(pid):
    return "rh-p%s" % pid


def channel_id(freq):
    return "rh-f%s" % freq


def _attr(obj, name, default=None):
    # Attributes first: from 4.2 a lap is a dataclass that also subclasses dict,
    # and its dict side is empty.
    if hasattr(obj, name):
        return getattr(obj, name)
    if isinstance(obj, dict):
        return obj.get(name, default)
    return default


def live_laps(rhapi):
    """The live race's crossings as a list (by seat) of lists of dicts.

    Read from the race object rather than RHAPI's race.laps_raw, whose shape
    has changed between releases (a dict by seat in 4.0 and 4.1, a list from
    4.3) and which raises an error on 4.2. The race object has held laps the
    same way since plugins arrived: a dict of seat -> crossings, each a dict
    (4.0, 4.1) or a dict-like dataclass (4.2 on).
    """
    node_laps = None
    try:
        node_laps = rhapi._racecontext.race.node_laps
    except Exception:
        try:
            node_laps = rhapi.race.laps_raw
        except Exception:
            node_laps = None
    if not node_laps:
        return []
    if isinstance(node_laps, dict):
        seats = [node_laps.get(i) or [] for i in range(max(node_laps.keys()) + 1)] if node_laps else []
    else:
        seats = list(node_laps)
    out = []
    for laps in seats:
        row = []
        for lap in laps or []:
            row.append({k: _attr(lap, k) for k in
                        ("lap_time_stamp", "lap_time", "deleted", "invalid", "late_lap")})
        out.append(row)
    return out


def monotonic_to_epoch_ms(rhapi, mono):
    """RotorHazard's internal race clock is time.monotonic(); convert to wall time."""
    if not mono:
        return 0
    try:
        return int(rhapi.server.monotonic_to_epoch_millis(mono))
    except Exception:
        # Before 4.2: the same conversion, done here.
        return int(_now_ms() - (monotonic() - float(mono)) * 1000)


def _hex_color(c):
    """Seat colours come back as '#rrggbb' strings or as packed ints."""
    if isinstance(c, str) and c.startswith("#"):
        return c
    try:
        return "#%06x" % (int(c) & 0xFFFFFF)
    except (TypeError, ValueError):
        return ""


class SnapshotBuilder:
    """Builds snapshots, remembering finished races between calls.

    A saved race does not change unless someone marshals it, and RotorHazard
    says so with an event when they do, so each one is translated once and
    reused until invalidate() is called. The live race is rebuilt every time,
    which is cheap: it is already in memory.
    """

    def __init__(self, rhapi):
        self.rhapi = rhapi
        self._saved = {}  # saved race id -> (race dict, [result dicts])

    def invalidate(self):
        self._saved = {}

    # ── helpers over RHAPI ──────────────────────────────────────────────

    def _option(self, name, default=None):
        try:
            v = self.rhapi.db.option(name, default)
        except Exception:
            return default
        return default if v in (None, "") else v

    def _seat_colors(self):
        """The colour of each seat as configured, whoever is in it.

        Not race.seat_colors: that greys out empty seats and follows the LED
        colour mode, and a channel's colour should stay put all meeting.
        """
        colors = None
        for getter in ("get", "get_item"):  # get_item before 4.3
            try:
                opt = getattr(self.rhapi.config, getter)("LED", "seatColors")
                if opt:
                    colors = [_hex_color(c) for c in opt]
                    break
            except Exception:
                pass
        if not colors:
            try:
                colors = list(self.rhapi.server.seat_color_defaults)
            except Exception:
                colors = None
        if not colors:
            try:
                colors = [_hex_color(c) for c in (self.rhapi.race.seat_colors or [])]
                colors = [c for c in colors if c and c != "#222222"]
            except Exception:
                colors = None
        return [c for c in (colors or []) if c] or list(_DEFAULT_COLORS)

    def _frequencies(self):
        """The current frequency set as [(freq, band, channel), ...] by seat."""
        out = []
        try:
            fs = self.rhapi.race.frequencyset
            data = json.loads(_attr(fs, "frequencies") or "{}")
        except Exception:
            return out
        freqs, bands, chans = data.get("f") or [], data.get("b") or [], data.get("c") or []
        for i, f in enumerate(freqs):
            b = bands[i] if i < len(bands) else None
            c = chans[i] if i < len(chans) else None
            out.append((f or 0, b, c))
        return out

    def _format(self, format_id=None):
        try:
            if format_id:
                f = self.rhapi.db.raceformat_by_id(format_id)
                if f:
                    return f
            return self.rhapi.race.raceformat
        except Exception:
            return None

    # ── the snapshot ────────────────────────────────────────────────────

    def build(self):
        rhapi = self.rhapi
        db = rhapi.db
        event_name = str(self._option("eventName", "") or "RotorHazard race")
        consec = int(self._option("consecutivesCount", 3) or 3)

        pilots = {p.id: p for p in (db.pilots or [])}
        classes = {c.id: c for c in (db.raceclasses or [])}
        heats = sorted(db.heats or [], key=lambda h: h.id)
        heat_by_id = {h.id: h for h in heats}

        # A heat's number within its class ("Race 3" of the Open class), the
        # way FPVTrackside numbers races within a round.
        heat_number = {}
        per_class = {}
        for h in heats:
            per_class.setdefault(h.class_id, 0)
            per_class[h.class_id] += 1
            heat_number[h.id] = per_class[h.class_id]

        colors = self._seat_colors()
        freqs = self._frequencies()
        channels = {}  # freq -> channel dict
        event_channels, event_colors = [], []

        def channel_for(freq, seat):
            """Channel id for a frequency, naming it from the current set."""
            if not freq:
                return ""
            cid = channel_id(freq)
            if freq not in channels:
                band, num = None, None
                for (f, b, c) in freqs:
                    if f == freq and b:
                        band, num = b, c
                        break
                if band is None:
                    for b, fl in _BANDS.items():
                        if freq in fl:
                            band, num = b, fl.index(freq) + 1
                            break
                display = "%s%s" % (band, num) if band and num else str(freq)
                channels[freq] = {
                    "ID": cid, "Number": int(num or 0), "Band": _BAND_NAMES.get(band, band or ""),
                    "ShortBand": band or "", "ChannelPrefix": band or "", "Frequency": int(freq),
                    "DisplayName": display,
                }
                event_channels.append(cid)
                event_colors.append(colors[seat % len(colors)] if seat is not None else "")
            return cid

        # Name every seat of the current frequency set first, so its colours
        # follow the seats the way they do on RotorHazard's own pages.
        for seat, (f, _b, _c) in enumerate(freqs):
            channel_for(f, seat)

        rounds = {}  # (class id, round) -> round dict
        order = [0]

        def round_for(class_id, num, etype):
            key = (class_id or 0, num)
            if key not in rounds:
                order[0] += 1
                cls = classes.get(class_id)
                cname = (cls.name if cls and cls.name else "")
                rounds[key] = {
                    "ID": "rh-c%s-r%s" % key, "Name": ("%s Round %s" % (cname, num)).strip(),
                    "RoundNumber": int(num), "EventType": etype, "RoundType": "Round",
                    "Valid": True, "Order": order[0],
                }
            return rounds[key]["ID"]

        def etype_for(fmt):
            if fmt is not None and _attr(fmt, "win_condition") == WIN_FASTEST_CONSECUTIVE:
                return "TimeTrial"
            return "Race"

        def target_laps(fmt):
            if fmt is None:
                return 0
            if _attr(fmt, "win_condition") == WIN_FIRST_TO_X:
                return int(_attr(fmt, "number_laps_win", 0) or 0)
            return 0

        races, results = [], []
        seen = set()

        # 1. Races already run, in the order they were run.
        for sr in sorted(db.races or [], key=lambda r: r.id):
            rid = race_id(sr.heat_id, sr.round_id)
            cached = self._saved.get(sr.id)
            if cached is None:
                cached = self._saved_race(sr, rid)
                self._saved[sr.id] = cached
            race, res, fmt_etype, used = cached
            for freq, seat in used:
                channel_for(freq, seat)
            race = dict(race)
            res = [dict(r) for r in res]
            race["RaceNumber"] = heat_number.get(sr.heat_id, 0)
            race["Round"] = round_for(sr.class_id, sr.round_id, fmt_etype)
            for r in res:
                r["Round"] = race["Round"]
            races.append(race)
            results.extend(res)
            seen.add(rid)

        # 2. The live race: cued, staging, running, or finished but unsaved.
        selected = ""
        live = self._live_race(pilots, channel_for)
        cur_heat = live["heat"] if live else None
        if live:
            fmt = self._format()
            race = live["race"]
            if cur_heat:
                race["RaceNumber"] = heat_number.get(cur_heat, 0)
                cls_id = heat_by_id[cur_heat].class_id if cur_heat in heat_by_id else None
                race["Round"] = round_for(cls_id, live["round"], etype_for(fmt))
            else:
                race["RaceNumber"] = 1
                key = ("practice", 0)
                if key not in rounds:
                    order[0] += 1
                    rounds[key] = {"ID": "rh-practice", "Name": "Practice", "RoundNumber": 0,
                                   "EventType": "Practice", "RoundType": "Round",
                                   "Valid": True, "Order": order[0]}
                race["Round"] = "rh-practice"
            race["TargetLaps"] = target_laps(fmt)
            if race["ID"] not in seen:
                races.append(race)
                seen.add(race["ID"])
                for r in live["results"]:
                    r["Round"] = race["Round"]
                results.extend(live["results"])
            if cur_heat:
                selected = race["ID"]

        # 3. The heats still to run after the current one, for "Up next".
        if cur_heat in heat_by_id:
            fmt = self._format()
            after = [h for h in heats if h.id > cur_heat] + [h for h in heats if h.id < cur_heat]
            added = 0
            for h in after:
                if added >= PENDING_HEATS:
                    break
                lineup = self._heat_lineup(h.id, pilots, freqs, channel_for)
                if not lineup:
                    continue  # a planned heat with nobody in it yet
                try:
                    rnd = int(db.heat_max_round(h.id) or 0) + 1
                except Exception:
                    rnd = 1
                rid = race_id(h.id, rnd)
                if rid in seen:
                    continue
                cls_fmt = None
                cls = classes.get(h.class_id)
                if cls is not None and _attr(cls, "format_id"):
                    cls_fmt = self._format(cls.format_id)
                f = cls_fmt or fmt
                races.append({
                    "ID": rid, "Laps": [], "Detections": [], "Start": "", "End": "",
                    "TotalPausedTime": "", "PilotChannels": lineup,
                    "RaceNumber": heat_number.get(h.id, 0),
                    "Round": round_for(h.class_id, rnd, etype_for(f)),
                    "TargetLaps": target_laps(f), "Valid": True, "Event": "", "Bracket": "",
                })
                seen.add(rid)
                added += 1

        event_id = "rh-" + hashlib.sha1(event_name.encode("utf-8")).hexdigest()[:12]
        for r in races:
            r["Event"] = event_id
        for r in results:
            r["Event"] = event_id

        starts = [r["Start"] for r in races if r.get("Start")]
        event = {
            "ID": event_id, "Name": event_name, "EventType": "Race",
            "Start": min(starts) if starts else utc(_now_ms()), "End": "",
            "Laps": consec, "PBLaps": consec, "PackLimit": 0,
            "PrimaryTimingSystemLocation": "RotorHazard",
            "Rounds": [r["ID"] for r in sorted(rounds.values(), key=lambda r: r["Order"])],
            "Races": [r["ID"] for r in races],
            "Channels": event_channels, "ChannelColors": event_colors,
        }
        pilot_list = []
        for p in pilots.values():
            name = (p.callsign or p.name or ("Pilot %s" % p.id)).strip()
            first, _, last = (p.name or "").strip().partition(" ")
            pilot_list.append({"ID": pilot_id(p.id), "Name": name, "FirstName": first,
                               "LastName": last, "DiscordID": "", "PracticePilot": False})
        for extra in (live or {}).get("seat_pilots", []):
            pilot_list.append(extra)

        return {
            "event": event,
            "channels": list(channels.values()),
            "pilots": pilot_list,
            "rounds": sorted(rounds.values(), key=lambda r: r["Order"]),
            "races": races,
            "results": results,
            "selected_race_id": selected,
        }

    # ── pieces ──────────────────────────────────────────────────────────

    def _saved_race(self, sr, rid):
        """A race already saved to the database, translated once.

        Returns the race, its results, its round type, and the (frequency,
        seat) pairs it used, so a reused race still names its channels.
        """
        db = self.rhapi.db
        fmt = self._format(sr.format_id)
        behaviour = _attr(fmt, "start_behavior", 0) if fmt is not None else 0
        start_ms = local_str_to_epoch_ms(sr.start_time_formatted)
        laps, dets, pcs, used = [], [], [], []
        last_ms = 0
        for run in db.pilotruns_by_race(sr.id) or []:
            if not run.pilot_id:
                continue
            pid = pilot_id(run.pilot_id)
            cid = channel_id(run.frequency) if run.frequency else ""
            used.append((run.frequency, run.node_index))
            pcs.append({"ID": "%s-pc%s" % (rid, run.node_index), "Pilot": pid, "Channel": cid})
            # Deleted laps travel as crossings that don't count, so dd-pits'
            # marshalling page can show them and put them back.
            every = sorted(db.laps_by_pilotrun(run.id) or [], key=lambda l: l.lap_time_stamp or 0)
            i = 0
            for lap in every:
                if lap.deleted:
                    dets.append({"ID": "rh-lap%s" % lap.id, "Channel": cid,
                                 "Time": utc(start_ms + int(lap.lap_time_stamp or 0)), "Pilot": pid,
                                 "LapNumber": -1, "Valid": False, "ValidityType": "Marshall",
                                 "IsLapEnd": True, "IsHoleshot": False,
                                 "TimingSystemIndex": 0, "RaceSector": 0})
                    continue
                last_ms = max(last_ms, int(lap.lap_time_stamp or 0))
                self._add_lap(laps, dets, rid, run.node_index, pid, cid, i, behaviour,
                              start_ms, lap.lap_time_stamp, lap.lap_time, det_id="rh-lap%s" % lap.id)
                i += 1
        race_ms = 0
        if fmt is not None and not _attr(fmt, "unlimited_time", 0):
            race_ms = int(_attr(fmt, "race_time_sec", 0) or 0) * 1000
        end_ms = start_ms + max(last_ms, race_ms, 1) if start_ms else 0
        race = {
            "ID": rid, "Laps": laps, "Detections": dets,
            "Start": utc(start_ms), "End": utc(end_ms), "TotalPausedTime": "",
            "PilotChannels": pcs, "TargetLaps": 0, "Valid": True, "Event": "", "Bracket": "",
        }
        tl = 0
        if fmt is not None and _attr(fmt, "win_condition") == WIN_FIRST_TO_X:
            tl = int(_attr(fmt, "number_laps_win", 0) or 0)
        race["TargetLaps"] = tl
        try:
            lb = db.race_results(sr)
        except Exception:
            lb = None
        results = _results(lb, rid)
        etype = "TimeTrial" if (fmt is not None and _attr(fmt, "win_condition") == WIN_FASTEST_CONSECUTIVE) else "Race"
        return race, results, etype, used

    def _live_race(self, pilots, channel_for):
        """The race RotorHazard has loaded now, whatever state it is in."""
        race = self.rhapi.race
        try:
            status = race.status
        except Exception:
            return None
        heat = race.heat or None
        if heat:
            try:
                rnd = int(race.round or 0)
            except Exception:
                rnd = 0
            if rnd <= 0:
                try:
                    rnd = int(self.rhapi.db.heat_max_round(heat) or 0) + 1
                except Exception:
                    rnd = 1
            rid = race_id(heat, rnd)
        else:
            rnd = 0
            if status in (READY,):
                return None  # practice mode with nothing running: no race to show
            rid = "rh-practice"
        fmt = self._format()
        behaviour = _attr(fmt, "start_behavior", 0) if fmt is not None else 0
        freqs = self._frequencies()

        start_ms = 0
        if status in (RACING, DONE):
            try:
                start_ms = monotonic_to_epoch_ms(self.rhapi, race.start_time_internal)
            except Exception:
                start_ms = 0
            if not start_ms:
                st = getattr(race, "start_time", None)
                start_ms = int(st.timestamp() * 1000) if hasattr(st, "timestamp") else 0
        end_ms = 0
        if status == DONE:
            try:
                end_ms = monotonic_to_epoch_ms(self.rhapi, race.end_time_internal)
            except Exception:
                end_ms = 0
            if not end_ms or end_ms < start_ms:
                end_ms = _now_ms()

        laps, dets, pcs, seat_pilots = [], [], [], []
        node_pilots = race.pilots or []
        if isinstance(node_pilots, dict):  # 4.0 keyed seats by index
            node_pilots = [node_pilots.get(i) for i in range(max(node_pilots.keys()) + 1)] if node_pilots else []
        node_pilots = list(node_pilots)
        raw = live_laps(self.rhapi)
        n_seats = max(len(node_pilots), len(raw), len(freqs))
        for seat in range(n_seats):
            p = node_pilots[seat] if seat < len(node_pilots) else None
            if p:
                pid = pilot_id(p)
            elif not heat and seat < len(raw) and raw[seat]:
                # Practice: a seat flying with no pilot named still has laps.
                pid = "rh-seat%s" % (seat + 1)
                seat_pilots.append({"ID": pid, "Name": "Seat %s" % (seat + 1), "FirstName": "",
                                    "LastName": "", "DiscordID": "", "PracticePilot": True})
            else:
                continue
            freq = freqs[seat][0] if seat < len(freqs) else 0
            cid = channel_for(freq, seat)
            pcs.append({"ID": "%s-pc%s" % (rid, seat), "Pilot": pid, "Channel": cid})
            if not start_ms:
                continue
            crossings = []
            for lap in (raw[seat] if seat < len(raw) else []):
                if lap.get("deleted") or lap.get("invalid") or lap.get("late_lap"):
                    continue
                crossings.append(lap)
            crossings.sort(key=lambda l: l.get("lap_time_stamp") or 0)
            for i, lap in enumerate(crossings):
                self._add_lap(laps, dets, rid, seat, pid, cid, i, behaviour, start_ms,
                              lap.get("lap_time_stamp"), lap.get("lap_time"))

        results = []
        if status == DONE:
            try:
                results = _results(race.results, rid)
            except Exception:
                results = []
        return {
            "heat": heat, "round": rnd, "results": results, "seat_pilots": seat_pilots,
            "race": {
                "ID": rid, "Laps": laps, "Detections": dets,
                "Start": utc(start_ms), "End": utc(end_ms), "TotalPausedTime": "",
                "PilotChannels": pcs, "TargetLaps": 0, "Valid": True, "Event": "", "Bracket": "",
            },
        }

    def _heat_lineup(self, heat_id, pilots, freqs, channel_for):
        out = []
        try:
            slots = self.rhapi.db.slots_by_heat(heat_id) or []
        except Exception:
            return out
        for s in sorted(slots, key=lambda s: s.node_index if s.node_index is not None else 99):
            if not s.pilot_id or s.pilot_id not in pilots:
                continue
            seat = s.node_index or 0
            freq = freqs[seat][0] if seat < len(freqs) else 0
            out.append({"ID": "rh-h%s-pc%s" % (heat_id, seat), "Pilot": pilot_id(s.pilot_id),
                        "Channel": channel_for(freq, seat)})
        return out

    @staticmethod
    def _add_lap(laps, dets, rid, seat, pid, cid, i, behaviour, start_ms, stamp, length, det_id=None):
        """One crossing as an FPVTrackside detection and lap.

        In a hole-shot race the first crossing starts the pilot's race and is
        lap 0, which dd-pits shows as the holeshot. When the format counts the
        first crossing as lap 1, it is numbered that way.
        """
        stamp = int(stamp or 0)
        length = int(length or 0)
        num = i + 1 if behaviour == START_FIRST_LAP else i
        end = start_ms + stamp
        did = det_id or "%s-d%s-%s" % (rid, seat, i)
        dets.append({"ID": did, "Channel": cid, "Time": utc(end), "Pilot": pid,
                     "LapNumber": num, "Valid": True, "ValidityType": "Auto",
                     "IsLapEnd": True, "IsHoleshot": num == 0,
                     "TimingSystemIndex": 0, "RaceSector": 0})
        laps.append({"ID": "%s-l%s-%s" % (rid, seat, i), "Detection": did,
                     "LengthSeconds": length / 1000.0, "LapNumber": num,
                     "StartTime": utc(end - length), "EndTime": utc(end)})


def _results(leaderboard, rid):
    """Finishing positions from a RotorHazard leaderboard."""
    out = []
    if not isinstance(leaderboard, dict):
        return out
    meta = leaderboard.get("meta") or {}
    key = meta.get("primary_leaderboard") or "by_race_time"
    rows = leaderboard.get(key) or []
    for row in rows:
        pos, pid = row.get("position"), row.get("pilot_id")
        if not pid or not isinstance(pos, int) or not row.get("laps"):
            continue
        out.append({"ID": "%s-res-%s" % (rid, pid), "Points": int(row.get("points") or 0),
                    "Position": pos, "Valid": True, "Event": "", "Pilot": pilot_id(pid),
                    "Race": rid, "Round": "", "DNF": False, "ResultType": "Race"})
    return out


def _now_ms():
    return int(datetime.now(timezone.utc).timestamp() * 1000)


def live_signature(rhapi):
    """A cheap fingerprint of the live race: anything dd-pits would redraw for.

    Read on every version poll, so it touches only what RotorHazard already
    holds in memory.
    """
    race = rhapi.race
    parts = []
    try:
        parts += [race.status, race.heat, getattr(race, "round", None)]
        parts.append(race.start_time_internal if race.status in (RACING, DONE) else 0)
        for seat in live_laps(rhapi):
            parts.append(len(seat))
            parts.append(sum(1 for l in seat if l.get("deleted")))
            if seat:
                parts.append(seat[-1].get("lap_time_stamp"))
        pilots = race.pilots or []
        parts.append(sorted(pilots.items()) if isinstance(pilots, dict) else list(pilots))
    except Exception as ex:  # never fail a poll over a fingerprint
        parts.append(repr(ex))
    return hashlib.sha1(repr(parts).encode("utf-8")).hexdigest()[:12]

