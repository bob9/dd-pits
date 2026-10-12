"""Marshalling a saved race from dd-pits: remove, restore or add a lap.

RotorHazard's Marshal page saves a pilot's corrected laps through the
server's resave_laps handler. dd-pits' marshalling goes through that same
handler, so results, heat and class caches, the race log and the LAPS_RESAVE
event all behave exactly as they do when the edit is made on the Marshal page.
"""

import sys

from .stages import StageError

MANUAL = 1  # Database.LapSource.MANUAL


def _resave_handler():
    srv = sys.modules.get("__main__")
    fn = getattr(srv, "on_resave_laps", None)
    if fn is None:
        srv = sys.modules.get("server")
        fn = getattr(srv, "on_resave_laps", None)
    if fn is None:
        raise StageError("this RotorHazard can't be marshalled from dd-pits")
    return fn


def marshal(rhapi, data):
    db = rhapi.db
    race = str(data.get("race") or "")
    try:
        heat_id, round_id = [int(x) for x in race[len("rh-h"):].split("-r")]
    except ValueError:
        raise StageError("marshal a saved race; save the race in RotorHazard first")
    saved = db.race_by_heat_round(heat_id, round_id)
    if saved is None:
        raise StageError("save the race in RotorHazard first, then marshal it")
    try:
        pilot_id = int(str(data.get("pilot") or "")[len("rh-p"):])
    except ValueError:
        raise StageError("no such pilot")
    run = None
    for r in db.pilotruns_by_race(saved.id) or []:
        if r.pilot_id == pilot_id:
            run = r
    if run is None:
        raise StageError("that pilot didn't fly this race")

    laps = []
    for lap in sorted(db.laps_by_pilotrun(run.id) or [], key=lambda l: l.lap_time_stamp or 0):
        laps.append({"id": lap.id, "lap_time_stamp": int(lap.lap_time_stamp or 0),
                     "lap_time": lap.lap_time, "source": lap.source,
                     "deleted": bool(lap.deleted), "peak_rssi": getattr(lap, "peak_rssi", None)})

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
    else:
        raise StageError("unknown action %r" % action)

    # Lap times follow from the crossings that count, as on the Marshal page.
    prev = 0
    for lap in laps:
        if lap["deleted"]:
            continue
        lap["lap_time"] = lap["lap_time_stamp"] - prev
        prev = lap["lap_time_stamp"]

    pilot = db.pilot_by_id(pilot_id)
    payload = {
        "heat_id": saved.heat_id, "round_id": saved.round_id,
        "callsign": getattr(pilot, "callsign", "") or "",
        "race_id": saved.id, "pilotrace_id": run.id,
        "node": run.node_index, "seat": run.node_index,  # "seat" from 4.4
        "pilot_id": pilot_id,
        "laps": [{k: v for k, v in l.items() if k != "id"} for l in laps],
        "enter_at": run.enter_at, "exit_at": run.exit_at,
    }
    _resave_handler()(payload)
    return {"ok": True}
