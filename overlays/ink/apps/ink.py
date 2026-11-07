"""Shows the shared ink canvas. The server sends both bit planes ready to copy into the display buffers.

This file is installed on its own onto stock badges, so it only imports modules the stock firmware has.
"""

import gc
import io
import json
import os
import time

import deflate
import network
import requests

from apps.base_app import BaseApp
from lib.button import UserA
from lib.epd import display
from lib.led_bar import leds

_CONFIG = "/data/ink.json"
# {"networks": [{"ssid": "...", "password": "..."}]}, or a single {"ssid": "...", "password": "..."}
_WIFI = "/data/wifi.json"
# The last frame as the server sent it (zlib, about 15 KB), shown at launch so the app works offline.
_CACHE = "/data/ink_frame.bin"
_RETRY_S = 20
_JOIN_MS = 15000

_wlan = None


def _networks():
    with open(_WIFI) as f:
        cfg = json.load(f)
    return cfg.get("networks") or [cfg]


def _connect():
    """Join the first saved network that is in range. Raises OSError on failure."""
    global _wlan
    if _wlan is None:
        _wlan = network.WLAN(network.STA_IF)
    if _wlan.isconnected():
        return
    _wlan.active(True)
    saved = _networks()
    seen = {ap[0].decode() for ap in _wlan.scan()}
    # A hidden network never shows up in a scan, so fall back to trying every saved one.
    candidates = [n for n in saved if n["ssid"] in seen] or saved
    for n in candidates:
        _wlan.connect(n["ssid"], n.get("password", ""))
        start = time.ticks_ms()
        while not _wlan.isconnected():
            if time.ticks_diff(time.ticks_ms(), start) > _JOIN_MS:
                _wlan.disconnect()
                break
            time.sleep_ms(100)
        else:
            return
    raise OSError("wifi: could not join " + ", ".join(n["ssid"] for n in candidates))


def _draw(frame):
    """Unpack a zlib frame into the display buffers."""
    with deflate.DeflateIO(io.BytesIO(frame), deflate.ZLIB) as z:
        _read_exact(z, display._framebuffer_black)
        _read_exact(z, display._framebuffer_red)


def _read_exact(stream, buf):
    view = memoryview(buf)
    pos = 0
    while pos < len(view):
        n = stream.readinto(view[pos:])
        if not n:
            raise OSError("frame ended early")
        pos += n


def _header(headers, name):
    # The server's header case differs from the spelling in HTTP specs.
    name = name.lower()
    for key, value in headers.items():
        if key.lower() == name:
            return value
    return None


class InkApp(BaseApp):
    def __init__(self) -> None:
        super().__init__("Ink")
        self.sleep_ms = 200
        try:
            with open(_CONFIG) as f:
                cfg = json.load(f)
        except OSError:
            cfg = {}
        self.url = cfg.get("server", "https://ink.underwhelm.ing").rstrip("/") + "/badge/frame"
        self.poll_s = cfg.get("poll_s", 60)
        self._etag = None
        self._next = 0

    def run(self):
        if self.launched:
            self._show_cached()
        if self.launched or UserA.pressed():
            # Another app may have drawn over the screen, so fetch even if unchanged.
            self._etag = None
            self._next = 0
        if time.time() < self._next or not display.is_done_updating():
            return
        try:
            changed = self._fetch()
            self._next = time.time() + self.poll_s
            if changed:
                display.update()
            leds.fill((0, 1, 0))
        except Exception as ex:
            print(f"ink: {ex}")
            leds.fill((1, 0, 0))
            self._next = time.time() + _RETRY_S

    def _fetch(self):
        """Download the frame into the display buffers. False when it hasn't changed."""
        leds.fill((0, 1, 1))
        _connect()
        headers = {}
        if self._etag:
            headers["If-None-Match"] = self._etag
        gc.collect()
        r = requests.get(self.url, headers=headers)
        try:
            if r.status_code == 304:
                return False
            if r.status_code != 200:
                raise OSError(f"http {r.status_code}")
            frame = r.content
            etag = _header(r.headers, "ETag")
        finally:
            r.close()
        _draw(frame)
        self._etag = etag
        try:
            with open(_CACHE + ".tmp", "wb") as f:
                f.write(frame)
            os.rename(_CACHE + ".tmp", _CACHE)
        except OSError as ex:
            print(f"ink: cache not saved: {ex}")
        return True

    def _show_cached(self):
        """Draw the last saved frame, if any, while the first fetch runs."""
        try:
            with open(_CACHE, "rb") as f:
                frame = f.read()
            _draw(frame)
        except Exception as ex:
            # No cache yet, or a damaged one: the fetch draws over it either way.
            print(f"ink: no cached frame: {ex}")
            return
        display.update()
