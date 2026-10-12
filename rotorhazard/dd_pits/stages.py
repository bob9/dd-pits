"""Create RotorHazard heats for dd-pits stages (brackets).

dd-pits runs the bracket; RotorHazard flies the races. When a bracket race's
line-up is known, dd-pits asks for it here and gets back the heat it became:
a heat in a race class named after the stage, with the pilots seated in
order. The heat's first run is the result dd-pits reads back.
"""

ASSIGN, NONE = 0, -1  # Database.ProgramMethod


def _program_methods():
    try:
        from Database import ProgramMethod
        return ProgramMethod.ASSIGN, ProgramMethod.NONE
    except Exception:
        return ASSIGN, NONE


class StageError(Exception):
    pass


FIRST_TO_LAP_X, MOST_PROGRESS = 2, 1  # RHRace.WinCondition


def _round_types():
    """Database.RoundType, or None before RotorHazard 4.2 (no heat groups)."""
    try:
        from Database import RoundType
        return RoundType
    except Exception:
        return None


def _use_groups(rhapi, cls):
    """Put a bracket's class into heat groups, one group per bracket round.

    rounds=1 stops RotorHazard copying each saved heat into the next group:
    the bracket decides who flies next, not a repeat of the same line-up.
    """
    rt = _round_types()
    if rt is None:
        return False
    if getattr(cls, "round_type", None) != rt.GROUPED or getattr(cls, "rounds", None) != 1:
        rhapi.db.raceclass_alter(cls.id, rounds=1, round_type=rt.GROUPED)
    return True


def _set_group(rhapi, heat_id, group):
    try:
        rhapi._racecontext.rhdata.alter_heat({"heat": heat_id, "group_id": group})
    except Exception:
        pass


def _find_class(rhapi, class_name, create=True):
    for c in rhapi.db.raceclasses or []:
        if (c.name or "").strip().lower() == class_name.lower():
            return c
    if not create:
        return None
    return rhapi.db.raceclass_add(name=class_name, description="Created by dd-pits for a bracket.")


def stage_format(rhapi, data, cls=None):
    """Give a bracket's race class the laps and race time set in dd-pits.

    The class gets a race format of its own, "dd-pits · <bracket>", copied
    from the timer's current format so staging tones, start behaviour and
    the rest stay as the club has them; only the laps and time are set.
    RotorHazard then runs every heat of the class with it. Nothing is
    changed when neither is set.
    """
    db = rhapi.db
    laps = int(data.get("laps") or 0)
    secs = int(data.get("race_time") or 0)
    class_name = (data.get("class") or "dd-pits stage").strip()
    if laps <= 0 and secs <= 0:
        return None
    if cls is None:
        cls = _find_class(rhapi, class_name)
    name = "dd-pits \u00b7 " + class_name
    fmt = None
    for f in db.raceformats or []:
        if (getattr(f, "name", "") or "") == name:
            fmt = f
    if fmt is None:
        base = None
        try:
            base = rhapi.race.raceformat
        except Exception:
            base = None
        try:
            fmt = db.raceformat_duplicate(base) if base is not None else None
        except Exception:
            fmt = None
        if fmt is None:
            fmt = db.raceformat_add(name=name)
    db.raceformat_alter(
        fmt.id, name=name,
        win_condition=FIRST_TO_LAP_X if laps > 0 else MOST_PROGRESS,
        number_laps_win=laps if laps > 0 else 0,
        unlimited_time=0 if secs > 0 else 1,
        race_time_sec=secs if secs > 0 else 0,
    )
    if getattr(cls, "format_id", None) != fmt.id:
        db.raceclass_alter(cls.id, raceformat=fmt.id)
    for fn in ("broadcast_raceformats", "broadcast_raceclasses"):
        try:
            getattr(rhapi.ui, fn)()
        except Exception:
            pass
    return fmt.id


def stage_heat(rhapi, data):
    """Create (or re-seat, if not yet run) the heat for one bracket race."""
    db = rhapi.db
    class_name = (data.get("class") or "dd-pits stage").strip()
    heat_name = (data.get("name") or "Race").strip()
    callsigns = [str(p).strip() for p in (data.get("pilots") or []) if str(p).strip()]
    if not callsigns:
        raise StageError("the race has no pilots")

    cls = _find_class(rhapi, class_name)
    stage_format(rhapi, data, cls)
    bracket_round = int(data.get("round") or 0)

    pilots = {}
    for p in db.pilots or []:
        for key in (p.callsign, p.name):
            if key and key.strip().lower() not in pilots:
                pilots[key.strip().lower()] = p.id
    ids = []
    for c in callsigns:
        pid = pilots.get(c.lower())
        if pid is None:
            pid = db.pilot_add(name=c, callsign=c).id
            pilots[c.lower()] = pid
        ids.append(pid)

    heat = None
    want = int(data.get("heat") or 0)
    if want:
        heat = db.heat_by_id(want)
        if heat is not None and (db.races_by_heat(heat.id) or []):
            return heat.id  # already flown: never re-seat a race that has run
    if heat is None:
        heat = db.heat_add(name=heat_name, raceclass=cls.id)
    elif heat.name != heat_name:
        db.heat_alter(heat.id, name=heat_name)
    # Grouped only once the class has a heat: RotorHazard's own group
    # priming fails on a class with none.
    grouped = _use_groups(rhapi, db.raceclass_by_id(cls.id) or cls)
    if grouped and bracket_round > 0 and getattr(heat, "group_id", 0) != bracket_round - 1:
        _set_group(rhapi, heat.id, bracket_round - 1)

    slots = sorted(db.slots_by_heat(heat.id) or [], key=lambda s: s.node_index if s.node_index is not None else 99)
    if len(ids) > len(slots):
        raise StageError("%s has %d pilots but the timer has %d seats" % (heat_name, len(ids), len(slots)))
    assign, none = _program_methods()
    for i, slot in enumerate(slots):
        if i < len(ids):
            db.slot_alter(slot.id, method=assign, pilot=ids[i])
        elif slot.pilot_id:
            db.slot_alter(slot.id, method=none)

    if data.get("current"):
        try:
            if rhapi.race.status == 0:  # READY: never switch heats mid-race
                rhapi.race.heat = heat.id
        except Exception:
            pass
    for fn in ("broadcast_pilots", "broadcast_raceclasses", "broadcast_heats", "broadcast_current_heat"):
        try:
            getattr(rhapi.ui, fn)()
        except Exception:
            pass
    return heat.id


READY, RACING, DONE, STAGING = 0, 1, 2, 3  # RHRace.RaceStatus


def race_control(rhapi, data):
    """Start, stop, restart or discard the race for a heat, from dd-pits.

    The same as the buttons on RotorHazard's Run page: the heat is selected,
    staging and the start tones run on the timer, and a stopped race is saved
    so its result reaches the bracket.
    """
    import gevent
    race = rhapi.race
    action = (data.get("action") or "").lower()
    heat = int(data.get("heat") or 0)

    def select():
        if heat and race.heat != heat:
            if race.status != READY:
                raise StageError("another race is on the timer; stop or save it first")
            race.heat = heat

    if action == "start":
        if race.status in (RACING, STAGING):
            raise StageError("a race is already running")
        if race.status == DONE:
            raise StageError("the last race hasn't been saved; save or discard it in RotorHazard first")
        select()
        race.stage()
    elif action == "stop":
        if race.status not in (RACING, STAGING):
            raise StageError("no race is running")
        race.stop(doSave=True)
    elif action == "discard":
        if race.status in (RACING, STAGING):
            race.stop()
        if race.status == DONE:
            race.clear()
    elif action == "restart":
        # Throw away the run in progress (or the unsaved one) and go again.
        if race.status in (RACING, STAGING):
            race.stop()
            gevent.sleep(0.2)
        if race.status == DONE:
            race.clear()
            gevent.sleep(0.2)
        # A grouped heat that has been saved can't be raced again, so a
        # re-run flies a fresh copy of it, in the same group.
        h = rhapi.db.heat_by_id(heat) if heat else None
        if h is not None and (rhapi.db.races_by_heat(h.id) or []):
            cls = rhapi.db.raceclass_by_id(h.class_id) if h.class_id else None
            rt = _round_types()
            if cls is not None and rt is not None and getattr(cls, "round_type", None) == rt.GROUPED:
                copy = rhapi._racecontext.rhdata.duplicate_heat(h, new_heat_name=h.name, group_id=h.group_id)
                heat = copy.id
                try:
                    rhapi.ui.broadcast_heats()
                except Exception:
                    pass
        select()
        race.stage()
    else:
        raise StageError("unknown action %r" % action)
    try:
        rhapi.ui.broadcast_race_status()
    except Exception:
        pass
    return {"status": race.status, "heat": race.heat}


def delete_heats(rhapi, data):
    """Remove a deleted bracket's heats that were never flown.

    A heat with a saved race keeps its results and stays, as does a heat
    being raced right now. A heat selected but idle is deselected first.
    """
    deleted, kept = [], []
    try:
        current = rhapi.race.heat
    except Exception:
        current = None
    for hid in data.get("heats") or []:
        try:
            hid = int(hid)
        except (TypeError, ValueError):
            continue
        heat = rhapi.db.heat_by_id(hid)
        if heat is None:
            continue
        if rhapi.db.races_by_heat(hid) or []:
            kept.append(hid)
            continue
        if hid == current:
            # Selected next: fine to remove while idle, never mid-race.
            if rhapi.race.status != READY:
                kept.append(hid)
                continue
            try:
                rhapi.race.heat = None  # back to practice until another heat is picked
            except Exception:
                kept.append(hid)
                continue
        rhapi.db.heat_delete(hid)
        deleted.append(hid)
    if deleted:
        try:
            rhapi.ui.broadcast_heats()
        except Exception:
            pass
    return {"deleted": deleted, "kept": kept}
