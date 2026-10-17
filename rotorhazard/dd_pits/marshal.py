"""Marshalling a saved race from dd-pits.

RotorHazard's Marshal page saves a pilot's corrected laps through the
server's resave_laps handler. dd-pits' marshalling goes through that same
handler, so results, heat and class caches, the race log and the LAPS_RESAVE
event all behave exactly as they do when the edit is made on the Marshal page
(and the FPVTrackside connector, listening for LAPS_RESAVE, carries the
correction back into FPVTrackside).

dd-pits reads each pilot's signal trace through traces() and does the
analysis itself: auto-detected lines, laps recalculated, the odd lap flagged.

A race is named either the dd-pits way ("rh-h<heat>-r<round>") or by
FPVTrackside's race ID ("ts:<guid>", the race attribute the connector keeps);
a pilot as "rh-p<id>" or "ts:<guid>".
"""

import json
import sys

from .stages import StageError

MANUAL = 1  # Database.LapSource.MANUAL


def _server_handler(name):
    for mod in ("__main__", "server"):
        fn = getattr(sys.modules.get(mod), name, None)
        if fn is not None:
            return fn
    raise StageError("this RotorHazard can't be marshalled from dd-pits")


def _find_race(db, ref):
    ref = str(ref or "")
    if ref.startswith("ts:"):
        want = ref[3:].lower()
        for race in db.races or []:
            if str(db.race_attribute_value(race.id, "trackside_race_ID") or "").lower() == want:
                return race
        raise StageError("that FPVTrackside race isn't saved on this RotorHazard yet")
    try:
        heat_id, round_id = [int(x) for x in ref[len("rh-h"):].split("-r")]
    except ValueError:
        raise StageError("marshal a saved race; save the race in RotorHazard first")
    saved = db.race_by_heat_round(heat_id, round_id)
    if saved is None:
        raise StageError("save the race in RotorHazard first, then marshal it")
    return saved


def _pilot_ref(db, pilot_id):
    return "rh-p%d" % pilot_id


def _find_run(db, saved, ref):
    ref = str(ref or "")
    runs = db.pilotruns_by_race(saved.id) or []
    if ref.startswith("ts:"):
        want = ref[3:].lower()
        for r in runs:
            if str(db.pilot_attribute_value(r.pilot_id, "trackside_pilot_ID", "") or "").lower() == want:
                return r
        raise StageError("that pilot didn't fly this race")
    try:
        pilot_id = int(ref[len("rh-p"):])
    except ValueError:
        raise StageError("no such pilot")
    for r in runs:
        if r.pilot_id == pilot_id:
            return r
    raise StageError("that pilot didn't fly this race")


def _laps(db, run):
    out = []
    for lap in sorted(db.laps_by_pilotrun(run.id) or [], key=lambda l: l.lap_time_stamp or 0):
        out.append({"id": lap.id, "lap_time_stamp": int(lap.lap_time_stamp or 0),
                    "lap_time": lap.lap_time, "source": lap.source,
                    "deleted": bool(lap.deleted), "peak_rssi": getattr(lap, "peak_rssi", None)})
    return out


def _json_list(raw):
    try:
        v = json.loads(raw or "[]")
        return v if isinstance(v, list) else []
    except Exception:
        return []


def _num(v, default=0.0):
    try:
        return float(v)
    except (TypeError, ValueError):
        return default


def traces(rhapi, data, server_config):
    """Every pilot's signal trace, lines and laps for one saved race, and the
    timer's lap rules: everything dd-pits needs to marshal it."""
    db = rhapi.db
    saved = _find_race(db, data.get("race"))
    start_delay_max = 0.0
    fmt = db.raceformat_by_id(saved.format_id) if getattr(saved, "format_id", None) else None
    if fmt is not None:
        start_delay_max = _num(getattr(fmt, "start_delay_max_ms", 0)) / 1000
    rules = {
        "min_lap_ms": _num(db.option("MinLapSec", 0)) * 1000,
        "min_lap_discard": str(server_config(rhapi, "TIMING", "MinLapBehavior", 0)) not in ("0", "", "False", "None"),
        "min_first_crossing_ms": _num(db.option("MinFirstCrossingSec", 0)) * 1000,
        "start_lower_pct": _num(server_config(rhapi, "TIMING", "startThreshLowerAmount", 0)),
        "start_lower_secs": _num(server_config(rhapi, "TIMING", "startThreshLowerDuration", 0)),
        "start_delay_max": start_delay_max,
    }
    pilots = []
    for run in db.pilotruns_by_race(saved.id) or []:
        if not run.pilot_id:
            continue
        pilot = db.pilot_by_id(run.pilot_id)
        pilots.append({
            "pilot": _pilot_ref(db, run.pilot_id),
            "ts_pilot_id": str(db.pilot_attribute_value(run.pilot_id, "trackside_pilot_ID", "") or ""),
            "callsign": getattr(pilot, "callsign", "") or "",
            "node": run.node_index,
            "frequency": getattr(run, "frequency", None),
            "enter_at": run.enter_at, "exit_at": run.exit_at,
            "history_times": _json_list(run.history_times),
            "history_values": _json_list(run.history_values),
            "laps": _laps(db, run),
        })
    return {"race": "rh-h%d-r%d" % (saved.heat_id, saved.round_id),
            "ts_race_id": str(db.race_attribute_value(saved.id, "trackside_race_ID") or ""),
            "start_time": saved.start_time, "rules": rules, "pilots": pilots}


def marshal(rhapi, data):
    """Change one pilot's laps: remove/restore/add one, or replace them all
    (with the lines that produced them)."""
    db = rhapi.db
    saved = _find_race(db, data.get("race"))
    run = _find_run(db, saved, data.get("pilot"))
    laps = _laps(db, run)
    enter_at, exit_at = run.enter_at, run.exit_at

    action = data.get("action")
    if action in ("invalidate", "validate"):
        target = str(data.get("lap") or "")
        hit = [l for l in laps if "rh-lap%s" % l["id"] == target]
        if not hit:
            raise StageError("no such lap")
        hit[0]["deleted"] = action == "invalidate"
    elif action == "add":
        ms = int(data.get("race_time_ms") or 0)
        if ms <= 0:
            raise StageError("give the lap's time from the race start")
        laps.append({"id": None, "lap_time_stamp": ms, "lap_time": 0, "source": MANUAL,
                     "deleted": False, "peak_rssi": None})
        laps.sort(key=lambda l: l["lap_time_stamp"])
    elif action == "replace":
        new = data.get("laps")
        if not isinstance(new, list):
            raise StageError("replace needs the full list of laps")
        laps = []
        for l in new:
            stamp = int(_num(l.get("lap_time_stamp")))
            if stamp <= 0:
                continue
            laps.append({"id": None, "lap_time_stamp": stamp, "lap_time": 0,
                         "source": int(_num(l.get("source"), 2)), "deleted": bool(l.get("deleted")),
                         "peak_rssi": l.get("peak_rssi")})
        laps.sort(key=lambda l: l["lap_time_stamp"])
        if data.get("enter_at") is not None:
            enter_at = int(_num(data.get("enter_at"), enter_at))
        if data.get("exit_at") is not None:
            exit_at = int(_num(data.get("exit_at"), exit_at))
    else:
        raise StageError("unknown action %r" % action)

    # Lap times follow from the crossings that count, as on the Marshal page.
    prev = 0
    for lap in laps:
        if lap["deleted"]:
            continue
        lap["lap_time"] = lap["lap_time_stamp"] - prev
        prev = lap["lap_time_stamp"]

    pilot = db.pilot_by_id(run.pilot_id)
    payload = {
        "heat_id": saved.heat_id, "round_id": saved.round_id,
        "callsign": getattr(pilot, "callsign", "") or "",
        "race_id": saved.id, "pilotrace_id": run.id,
        "node": run.node_index, "seat": run.node_index,  # "seat" from 4.4
        "pilot_id": run.pilot_id,
        "laps": [{k: v for k, v in l.items() if k != "id"} for l in laps],
        "enter_at": enter_at, "exit_at": exit_at,
    }
    _server_handler("on_resave_laps")(payload)
    return {"ok": True}


def calibrate(rhapi, data):
    """Use these lines on a node from now on (its enter/exit levels)."""
    try:
        node = int(data.get("node"))
        enter_at = int(_num(data.get("enter_at")))
        exit_at = int(_num(data.get("exit_at")))
    except (TypeError, ValueError):
        raise StageError("give the node and both lines")
    if not 0 < exit_at < enter_at:
        raise StageError("the exit line must be below the enter line")
    _server_handler("on_set_enter_at_level")({"node": node, "enter_at_level": enter_at})
    _server_handler("on_set_exit_at_level")({"node": node, "exit_at_level": exit_at})
    return {"ok": True}
