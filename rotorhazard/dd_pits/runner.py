"""Download, start and look after the dd-pits program on the timer.

dd-pits itself is a single Go binary. The plugin fetches the right build for
this machine from GitHub the first time it is needed (or uses one you point it
at), runs it next to RotorHazard, and restarts it if it stops.
"""

import hashlib
import logging
import os
import platform
import secrets
import stat
import subprocess
import sys
import time
import urllib.request

import gevent

logger = logging.getLogger(__name__)

RELEASES = "https://github.com/bob9/dd-pits/releases/latest/download/"


def asset_name():
    """The release file built for this operating system and processor."""
    system = platform.system().lower()
    machine = platform.machine().lower()
    if machine in ("x86_64", "amd64"):
        arch = "amd64"
    elif machine in ("aarch64", "arm64"):
        arch = "arm64"
    elif machine.startswith("armv") or machine == "arm":
        arch = "arm"  # 32-bit Raspberry Pi OS, on any Pi
    else:
        raise RuntimeError("dd-pits has no build for this processor (%s)" % machine)
    if system == "linux":
        if arch == "arm" and sys.maxsize > 2 ** 32:
            arch = "arm64"
        return "dd-pits-linux-" + arch
    if system == "darwin":
        return "dd-pits-darwin-" + ("arm64" if arch == "arm64" else "amd64")
    if system == "windows":
        return "dd-pits-windows-amd64.exe"
    raise RuntimeError("dd-pits has no build for %s" % platform.system())


class Runner:
    def __init__(self, home, rh_url, log_path):
        self.home = home
        self.rh_url = rh_url
        self.log_path = log_path
        self.proc = None
        self.wanted = False
        self.status = "stopped"
        self.last_error = ""
        self.port = 8088
        self.binary_override = ""
        self._watch = None
        os.makedirs(home, exist_ok=True)

    # ── files ───────────────────────────────────────────────────────────

    @property
    def binary(self):
        if self.binary_override:
            return self.binary_override
        name = "dd-pits.exe" if platform.system() == "Windows" else "dd-pits"
        return os.path.join(self.home, name)

    @property
    def config_path(self):
        return os.path.join(self.home, "dd-pits-local.json")

    def setup_pin(self):
        """A PIN for changing dd-pits settings from another device.

        The timer has no screen of its own, so every settings page is opened
        from a laptop or phone, and dd-pits only allows that with a PIN. The
        plugin makes one up once and hands it over only to RotorHazard admins.
        """
        path = os.path.join(self.home, "setup-pin.txt")
        try:
            with open(path) as f:
                pin = f.read().strip()
                if pin:
                    return pin
        except OSError:
            pass
        pin = "%06d" % secrets.randbelow(10 ** 6)
        with open(path, "w") as f:
            f.write(pin + "\n")
        return pin

    def installed_version(self):
        if not os.path.exists(self.binary):
            return ""
        try:
            out = subprocess.run([self.binary, "-version"], capture_output=True, text=True, timeout=10)
            return out.stdout.strip()
        except Exception as ex:
            return "unreadable (%s)" % ex

    def download(self):
        """Fetch the latest dd-pits for this machine, checking its checksum."""
        if self.binary_override:
            raise RuntimeError("using your own dd-pits at %s, so not downloading one" % self.binary_override)
        asset = asset_name()
        logger.info("dd-pits: downloading %s", RELEASES + asset)
        self.status = "downloading"
        data = _fetch(RELEASES + asset)
        sums = _fetch(RELEASES + "SHA256SUMS.txt").decode("utf-8", "replace")
        want = ""
        for line in sums.splitlines():
            parts = line.split()
            if len(parts) == 2 and parts[1].lstrip("*") == asset:
                want = parts[0].lower()
        got = hashlib.sha256(data).hexdigest()
        if not want:
            raise RuntimeError("%s is not listed in the release checksums" % asset)
        if got != want:
            raise RuntimeError("download of %s failed its checksum" % asset)
        tmp = self.binary + ".new"
        with open(tmp, "wb") as f:
            f.write(data)
        os.chmod(tmp, os.stat(tmp).st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
        os.replace(tmp, self.binary)
        logger.info("dd-pits: installed %s", self.installed_version() or asset)

    # ── process ─────────────────────────────────────────────────────────

    def running(self):
        return self.proc is not None and self.proc.poll() is None

    def start(self):
        """Start dd-pits, downloading it first if it is not here yet."""
        self.wanted = True
        if self.running():
            return
        try:
            if not os.path.exists(self.binary):
                self.download()
            pin = self.setup_pin()
            args = [self.binary,
                    "-rotorhazard", self.rh_url,
                    "-serve-addr", ":%d" % int(self.port),
                    "-local-config", self.config_path,
                    "-setup-pin", pin]
            logger.info("dd-pits: starting on port %s", self.port)
            log = open(self.log_path, "ab")
            kwargs = {}
            if os.name == "posix":
                kwargs["start_new_session"] = True
                if platform.system() == "Linux":
                    kwargs["preexec_fn"] = _die_with_parent
            self.proc = subprocess.Popen(args, cwd=self.home, stdout=log, stderr=subprocess.STDOUT,
                                         stdin=subprocess.DEVNULL, **kwargs)
            log.close()
            self.status = "running"
            self.last_error = ""
        except Exception as ex:
            self.status = "failed"
            self.last_error = str(ex)
            logger.warning("dd-pits: could not start: %s", ex)
        if self._watch is None:
            self._watch = gevent.spawn(self._supervise)

    def stop(self):
        self.wanted = False
        p = self.proc
        if p is None or p.poll() is not None:
            self.status = "stopped"
            return
        logger.info("dd-pits: stopping")
        try:
            p.terminate()  # SIGTERM on Linux and macOS: dd-pits shuts down cleanly
        except Exception:
            pass
        for _ in range(50):
            if p.poll() is not None:
                break
            gevent.sleep(0.1)
        if p.poll() is None:
            p.kill()
        self.status = "stopped"

    def restart(self):
        self.stop()
        self.start()

    def _supervise(self):
        """Bring dd-pits back if it stops while it is meant to be running."""
        backoff = 2
        while True:
            gevent.sleep(2)
            if not self.wanted or self.proc is None:
                continue
            code = self.proc.poll()
            if code is None:
                backoff = 2
                continue
            self.status = "restarting"
            self.last_error = "dd-pits stopped (exit code %s); restarting" % code
            logger.warning("dd-pits: %s", self.last_error)
            gevent.sleep(backoff)
            backoff = min(backoff * 2, 60)
            if self.wanted and not self.running():
                self.start()

    def tail_log(self, lines=40):
        try:
            with open(self.log_path, "rb") as f:
                f.seek(0, os.SEEK_END)
                size = f.tell()
                f.seek(max(0, size - 16384))
                text = f.read().decode("utf-8", "replace")
        except OSError:
            return ""
        return "\n".join(text.splitlines()[-lines:])


def _die_with_parent():
    """Ask Linux to stop dd-pits if RotorHazard dies without shutting it down.

    A crashed or killed RotorHazard never runs its shutdown event, and an
    orphaned dd-pits would hold the port against the next one.
    """
    try:
        import ctypes
        PR_SET_PDEATHSIG = 1
        ctypes.CDLL("libc.so.6", use_errno=True).prctl(PR_SET_PDEATHSIG, 15)  # SIGTERM
    except Exception:
        pass


def _fetch(url, timeout=120):
    req = urllib.request.Request(url, headers={"User-Agent": "dd-pits-rotorhazard-plugin"})
    start = time.time()
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        data = resp.read()
    logger.debug("dd-pits: fetched %s (%d bytes) in %.1fs", url, len(data), time.time() - start)
    return data
