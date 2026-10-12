"""dd-pits for RotorHazard.

Runs dd-pits on the RotorHazard timer and feeds it RotorHazard's timing, so a
club racing on RotorHazard gets everything dd-pits does: the live dashboard on
every phone, the TV view, 22 OBS overlays, spoken race calls, AI commentary,
and lap times in pilots' goggles over ELRS.

The plugin does three things:

* serves RotorHazard's meeting at /dd-pits/api/... in the shape dd-pits reads
  (see snapshot.py);
* downloads dd-pits for this machine and keeps it running (see runner.py);
* adds a dd-pits panel to RotorHazard's Settings page, with links to the
  dashboard, the overlays and dd-pits' own settings.

Free and open source (MIT), by bob9: https://github.com/bob9/dd-pits
"""

import dataclasses
import logging
import os

from flask import Blueprint, Response, jsonify, redirect, request
from eventmanager import Evt
from RHUI import UIField, UIFieldType

from .runner import Runner
from .snapshot import SnapshotBuilder, live_signature
from .marshal import marshal
from .stages import StageError, race_control, stage_heat

logger = logging.getLogger(__name__)

PANEL = "dd_pits"
SECTION = "DD_PITS"
SUPPORT_URL = "https://buymeacoffee.com/bobnine"
PROJECT_URL = "https://github.com/bob9/dd-pits"

def _events(*names):
    """Event ids by name, skipping any this RotorHazard release does not have.

    The event list grows with every release, so naming one that an older timer
    lacks must not stop the plugin loading.
    """
    return {getattr(Evt, n) for n in names if hasattr(Evt, n)}


# Events that only touch the live race. The live race is fingerprinted on every
# poll anyway (live_signature), so these never invalidate the finished races.
_LIVE_EVENTS = _events(
    "RACE_SCHEDULE", "RACE_SCHEDULE_CANCEL", "RACE_STAGE", "RACE_ABORT",
    "RACE_STAGE_TONE", "RACE_START", "RACE_CLOCK_CALLOUT", "RACE_FINISH",
    "RACE_STOP", "RACE_WIN", "RACE_FIRST_PASS", "RACE_LAP_RECORDED",
    "RACE_PILOT_LEADING", "RACE_PILOT_DONE", "CROSSING_ENTER", "CROSSING_EXIT",
    "RACE_INITIAL_PASS", "RACE_LAPS_REPLACE", "LAPS_DISCARD", "LAPS_CLEAR",
    "LAP_DELETE", "LAP_RESTORE_DELETED", "HEARTBEAT",
    "MESSAGE_STANDARD", "MESSAGE_INTERRUPT", "UI_DISPATCH",
    "LED_EFFECT_SET", "LED_BRIGHTNESS_SET", "LED_MANUAL", "LED_SET_MANUAL",
    "VRX_DATA_RECEIVE", "CACHE_READY", "CACHE_FAIL",
)

# Events that add a race without changing the ones already translated.
_APPEND_EVENTS = _events("LAPS_SAVE")

# Settings live in RotorHazard's config file from 4.3 (persistent_section);
# before that, as options in the event database.
_PERSISTENT = "persistent_section" in {f.name for f in dataclasses.fields(UIField)} \
    if dataclasses.is_dataclass(UIField) else False

_SETTING_DEFAULTS = {"autostart": "1", "port": "8088", "binary": "", "public_url": "", "menu": "1"}

# The links added to RotorHazard's own menu, so nobody has to know the port.
_NAV_MAIN = ('<li class="dd-pits-nav"><a href="/dd-pits/">dd-pits</a></li>'
             '<li class="dd-pits-nav"><a href="/dd-pits/upcoming">Upcoming</a></li>')
_NAV_EXTRA = ('<li class="dd-pits-nav"><a href="/dd-pits/stages">dd-pits brackets</a></li>'
              '<li class="dd-pits-nav"><a href="/dd-pits/overlays">dd-pits OBS overlays</a></li>')


def server_config(rhapi, section, name, default=None):
    """A value from RotorHazard's config, on any 4.x release.

    rhapi.config.get arrived in 4.3; 4.1 and 4.2 call it get_item; 4.0 kept
    the config in a module of its own, with the admin login under GENERAL
    rather than SECRETS.
    """
    cfg = getattr(rhapi, "config", None)
    for getter in ("get", "get_item"):
        try:
            v = getattr(cfg, getter)(section, name)
            if v not in (None, "", False):  # a missing key reads as False
                return v
        except Exception:
            pass
    try:
        import Config  # RotorHazard 4.0
        v = getattr(Config, section, {}).get(name)
        if v in (None, "") and section == "SECRETS":
            v = getattr(Config, "GENERAL", {}).get(name)
        if v is not None:
            return v
    except Exception:
        pass
    return default


def _truthy(v):
    return str(v).strip().lower() in ("1", "true", "yes", "on")


class DDPits:
    def __init__(self, rhapi):
        self.rhapi = rhapi
        self.builder = SnapshotBuilder(rhapi)
        self.generation = 0
        self._cache_key = None
        self._cache = None
        try:
            home = os.path.join(rhapi.server.data_dir, "dd-pits")
        except Exception:
            # Before 4.3 there is no data directory, and plugins live inside the
            # RotorHazard install, which an update replaces — so keep dd-pits
            # and its settings in the home directory instead.
            home = os.path.expanduser("~/dd-pits-rotorhazard")
        try:
            rh_port = int(server_config(rhapi, "GENERAL", "HTTP_PORT", 5000) or 5000)
        except (TypeError, ValueError):
            rh_port = 5000
        self.runner = Runner(home, "http://127.0.0.1:%d" % rh_port, os.path.join(home, "dd-pits.log"))

    # ── settings ────────────────────────────────────────────────────────

    def setting(self, name, default=""):
        v = None
        if _PERSISTENT:
            v = server_config(self.rhapi, SECTION, name)
        else:
            try:
                v = self.rhapi.db.option("dd_pits_" + name, None)
            except Exception:
                v = None
        return default if v in (None, "", False) else v

    def port(self):
        try:
            return int(self.setting("port", 8088))
        except ValueError:
            return 8088

    def apply_settings(self):
        self.runner.port = self.port()
        self.runner.binary_override = str(self.setting("binary", "")).strip()

    # ── events ──────────────────────────────────────────────────────────

    def on_any_event(self, args):
        name = args.get("_eventName")
        if name in _LIVE_EVENTS:
            return
        self.generation += 1
        if name not in _APPEND_EVENTS:
            self.builder.invalidate()

    def on_startup(self, _args):
        self.apply_settings()
        self.render_panel()
        if _truthy(self.setting("autostart", "1")):
            self.runner.start()
            self.render_panel()

    def on_shutdown(self, _args):
        self.runner.stop()

    # ── API for dd-pits ─────────────────────────────────────────────────

    def version(self):
        return "%d.%s" % (self.generation, live_signature(self.rhapi))

    def snapshot(self):
        key = self.version()
        if key != self._cache_key or self._cache is None:
            self._cache = self.builder.build()
            self._cache_key = key
        return self._cache

    # ── buttons ─────────────────────────────────────────────────────────

    def btn_start(self, _args=None):
        self.apply_settings()
        self.runner.restart() if self.runner.running() else self.runner.start()
        self._notify()

    def btn_stop(self, _args=None):
        self.runner.stop()
        self._notify()

    def btn_update(self, _args=None):
        self.apply_settings()
        was_running = self.runner.running()
        try:
            self.runner.stop()
            self.runner.download()
            self.rhapi.ui.message_notify("dd-pits updated: %s" % (self.runner.installed_version() or "latest"))
        except Exception as ex:
            self.runner.last_error = str(ex)
            self.rhapi.ui.message_alert("dd-pits update failed: %s" % ex)
        if was_running or _truthy(self.setting("autostart", "1")):
            self.runner.start()
        self._notify()

    def _notify(self):
        self.render_panel()
        try:
            self.rhapi.ui.broadcast_ui("settings")
        except Exception:
            pass
        r = self.runner
        if r.status == "failed":
            self.rhapi.ui.message_alert("dd-pits could not start: %s" % r.last_error)
        else:
            self.rhapi.ui.message_notify("dd-pits is %s" % r.status)

    # ── panel ───────────────────────────────────────────────────────────

    def render_panel(self):
        if not hasattr(self.rhapi.ui, "register_markdown"):
            return  # before 4.2: the links are in the settings' descriptions instead
        r = self.runner
        state = {
            "running": "**Running** on port %d." % r.port,
            "stopped": "**Stopped.**",
            "downloading": "**Downloading dd-pits…**",
            "restarting": "**Restarting…**",
            "failed": "**Could not start.**",
        }.get(r.status, r.status)
        if r.last_error:
            state += " %s" % r.last_error
        public = str(self.setting("public_url", "")).strip()
        if public:
            where = "Pilots and spectators open **%s**." % public
        else:
            where = "Pilots and spectators open **http://&lt;this timer&gt;:%d** on the venue wifi." % r.port
        md = (
            "dd-pits turns this timer's race data into a live dashboard for every phone, "
            "a TV view, OBS overlays, spoken race calls, AI commentary and lap times in "
            "pilots' goggles.\n\n"
            "Status: %s\n\n"
            "* [Open the live dashboard](/dd-pits/) to share with pilots\n"
            "* [Set up dd-pits](/dd-pits/setup): race calls, commentary, goggles\n"
            "* [OBS overlays](/dd-pits/overlays) for your stream\n"
            "* [Brackets](/dd-pits/stages): double elimination, qualifiers, ladders\n"
            "* [Upcoming races](/dd-pits/upcoming): every race still to run\n\n"
            "%s\n\n"
            "Free and open source by [bob9](%s). If it helps your club, "
            "[buy me a coffee](%s) ☕"
        ) % (state, where, PROJECT_URL, SUPPORT_URL)
        self.rhapi.ui.register_markdown(PANEL, "dd_pits_status", md)

    def _field(self, name, label, ftype, **kw):
        """A settings field, stored where this RotorHazard release keeps them."""
        kw.setdefault("value", _SETTING_DEFAULTS.get(name, ""))
        if _PERSISTENT:
            return UIField(name, label, ftype, persistent_section=SECTION, **kw)
        return UIField("dd_pits_" + name, label, ftype, **kw)

    def register_ui(self):
        ui, fields = self.rhapi.ui, self.rhapi.fields
        if _PERSISTENT:
            try:
                # Unregistered sections are cleaned out of config.json at startup.
                self.rhapi.config.register_section(SECTION)
            except Exception:
                pass
        ui.register_panel(PANEL, "dd-pits: dashboard, overlays & race calls", "settings")
        links = ""
        if not hasattr(ui, "register_markdown"):
            links = (" Dashboard: http://<this timer>:<port>. dd-pits settings: <this timer>/dd-pits/setup. "
                     "OBS overlays: <this timer>/dd-pits/overlays.")
        fields.register_option(self._field(
            "autostart", "Start dd-pits with RotorHazard", UIFieldType.CHECKBOX,
            desc="Runs dd-pits whenever the timer starts. The first start downloads it, so do that with internet." + links), PANEL)
        fields.register_option(self._field(
            "port", "dd-pits port", getattr(UIFieldType, "BASIC_INT", UIFieldType.TEXT),
            desc="Pilots open http://<this timer>:<port>. Press Start / restart after changing it."), PANEL)
        fields.register_option(self._field(
            "public_url", "Public dashboard address", UIFieldType.TEXT,
            placeholder="leave empty at a venue",
            desc="Only for a timer published on the internet behind a proxy: where dd-pits is reached, "
                 "either a path on this site (/pits) or its own address (https://pits.example.com). "
                 "The links in RotorHazard then use it."), PANEL)
        fields.register_option(self._field(
            "menu", "Show dd-pits in RotorHazard's menu", UIFieldType.CHECKBOX,
            desc="Adds dd-pits to the menu at the top of every RotorHazard page, and its brackets and "
                 "OBS overlays to the ⋮ menu."), PANEL)
        fields.register_option(self._field(
            "binary", "Use my own dd-pits program", UIFieldType.TEXT,
            placeholder="leave empty to download it automatically",
            desc="Full path to a dd-pits binary, for a timer with no internet or a custom build."), PANEL)
        ui.register_quickbutton(PANEL, "dd_pits_start", "Start / restart dd-pits", self.btn_start)
        ui.register_quickbutton(PANEL, "dd_pits_stop", "Stop dd-pits", self.btn_stop)
        ui.register_quickbutton(PANEL, "dd_pits_update", "Update dd-pits", self.btn_update)
        self.render_panel()

    # ── pages ───────────────────────────────────────────────────────────

    def _admin_ok(self, require=False):
        """RotorHazard's own admin login, as its Settings page checks it.

        With require, a timer with no admin login set does not count: an
        unprotected timer must not let anyone on the network change its heats.
        """
        user = server_config(self.rhapi, "SECRETS", "ADMIN_USERNAME", "")
        pw = server_config(self.rhapi, "SECRETS", "ADMIN_PASSWORD", "")
        if not user and not pw:
            return not require
        auth = request.authorization
        return auth is not None and auth.username == user and auth.password == pw

    def _add_menu_links(self, resp):
        """Put dd-pits in RotorHazard's own menu, on every page that has it.

        RotorHazard has no way for a plugin to add a menu item, so the links
        go into the page as it is sent. Only whole HTML pages with RotorHazard's
        menu are touched, and only once.
        """
        try:
            if (resp.status_code != 200 or resp.direct_passthrough or resp.is_streamed
                    or not (resp.mimetype or "").startswith("text/html")
                    or not _truthy(self.setting("menu", "1"))):
                return resp
            html = resp.get_data(as_text=True)
            if 'id="nav-main"' not in html or "dd-pits-nav" in html:
                return resp
            html = html.replace('<li class="extra">', _NAV_MAIN + '<li class="extra">', 1)
            html = html.replace('<li><a href="/streams">', _NAV_EXTRA + '<li><a href="/streams">', 1)
            resp.set_data(html)
        except Exception as ex:  # never break a RotorHazard page over a link
            logger.debug("dd-pits: menu link not added: %s", ex)
        return resp

    def _dd_url(self, path):
        # A timer published on the internet behind a proxy gives dd-pits its own
        # address; at a venue, it is this timer on dd-pits' port.
        public = str(self.setting("public_url", "")).strip().rstrip("/")
        if public.startswith("/"):
            return public + path  # a path on this same site, e.g. /pits
        if public:
            if "://" not in public:
                public = "https://" + public
            return public + path
        host = request.host.rsplit(":", 1)[0] if not request.host.endswith("]") else request.host
        return "%s://%s:%d%s" % (request.scheme, host, self.port(), path)

    def blueprint(self):
        bp = Blueprint("dd_pits", __name__, url_prefix="/dd-pits")

        @bp.route("/api/version")
        def api_version():
            return jsonify({"version": self.version()})

        @bp.route("/api/status")
        def api_status():
            r = self.runner
            return jsonify({"status": r.status, "running": r.running(), "error": r.last_error,
                            "port": r.port, "autostart": _truthy(self.setting("autostart", "1")),
                            "persistent_settings": _PERSISTENT})

        @bp.route("/api/stage-heat", methods=["POST"])
        def api_stage_heat():
            # Changes RotorHazard's data, so only dd-pits (which holds the token
            # the plugin gave it) or a RotorHazard admin may call it.
            auth = request.headers.get("Authorization", "")
            if auth != "Bearer " + self.runner.api_token() and not self._admin_ok(require=True):
                return jsonify({"error": "not allowed"}), 401
            try:
                heat = stage_heat(self.rhapi, request.get_json(force=True) or {})
            except StageError as ex:
                return jsonify({"error": str(ex)}), 409
            self.generation += 1
            return jsonify({"heat": heat})

        @bp.route("/api/race-control", methods=["POST"])
        def api_race_control():
            auth = request.headers.get("Authorization", "")
            if auth != "Bearer " + self.runner.api_token() and not self._admin_ok(require=True):
                return jsonify({"error": "not allowed"}), 401
            try:
                out = race_control(self.rhapi, request.get_json(force=True) or {})
            except StageError as ex:
                return jsonify({"error": str(ex)}), 409
            return jsonify(out)

        @bp.route("/api/marshal", methods=["POST"])
        def api_marshal():
            auth = request.headers.get("Authorization", "")
            if auth != "Bearer " + self.runner.api_token() and not self._admin_ok(require=True):
                return jsonify({"error": "not allowed"}), 401
            try:
                out = marshal(self.rhapi, request.get_json(force=True) or {})
            except StageError as ex:
                return jsonify({"error": str(ex)}), 409
            self.generation += 1
            self.builder.invalidate()
            return jsonify(out)

        @bp.route("/api/snapshot")
        def api_snapshot():
            return jsonify(self.snapshot())

        @bp.route("/")
        def dashboard():
            return redirect(self._dd_url("/"))

        @bp.route("/overlays")
        def overlays():
            return redirect(self._dd_url("/overlays"))

        @bp.route("/stages")
        def stages():
            return redirect(self._dd_url("/stages"))

        @bp.route("/upcoming")
        def upcoming():
            return redirect(self._dd_url("/dronedash/upcoming"))

        @bp.after_app_request
        def add_menu_links(resp):
            return self._add_menu_links(resp)

        @bp.route("/setup")
        def setup():
            if not self._admin_ok():
                return Response("Log in to RotorHazard to set up dd-pits.", 401,
                                {"WWW-Authenticate": 'Basic realm="Login Required"'})
            return redirect(self._dd_url("/setup?pin=%s" % self.runner.setup_pin()))

        @bp.route("/log")
        def log_page():
            if not self._admin_ok():
                return Response("Log in to RotorHazard to see the dd-pits log.", 401,
                                {"WWW-Authenticate": 'Basic realm="Login Required"'})
            return Response(self.runner.tail_log(200), mimetype="text/plain")

        return bp


def initialize(rhapi):
    dd = DDPits(rhapi)
    dd.register_ui()
    rhapi.ui.blueprint_add(dd.blueprint())
    rhapi.events.on(Evt.ALL, dd.on_any_event, name="dd_pits_changes", priority=50)
    rhapi.events.on(Evt.STARTUP, dd.on_startup, name="dd_pits_startup", priority=200)
    rhapi.events.on(Evt.SHUTDOWN, dd.on_shutdown, name="dd_pits_shutdown", priority=50)
