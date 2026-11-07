"""Link to the Quadrant site: a room the web player joins, a clock shared with the server, and run upload."""

import json
import time

import network
import requests

WIFI_PATH = "/data/wifi.json"
JOIN_MS = 15000
TIMEOUT_S = 3

_wlan = None


def connect():
    """Join the first saved network in range. Raises OSError on failure."""
    global _wlan
    if _wlan is None:
        _wlan = network.WLAN(network.STA_IF)
    if _wlan.isconnected():
        return
    _wlan.active(True)
    with open(WIFI_PATH) as f:
        cfg = json.load(f)
    saved = cfg.get("networks") or [cfg]
    seen = {ap[0].decode() for ap in _wlan.scan()}
    # A hidden network never shows up in a scan, so fall back to trying every saved one.
    for n in [n for n in saved if n["ssid"] in seen] or saved:
        _wlan.connect(n["ssid"], n.get("password", ""))
        start = time.ticks_ms()
        while not _wlan.isconnected():
            if time.ticks_diff(time.ticks_ms(), start) > JOIN_MS:
                _wlan.disconnect()
                break
            time.sleep_ms(100)
        else:
            return
    raise OSError("wifi: no saved network joined")


class Link:
    def __init__(self, server):
        self.server = server.rstrip("/")
        self.host = self.server.split("://", 1)[-1]
        self.code = None
        self.token = None
        self.base_ms = 0
        self.base_server = 0
        self.listeners = 0

    def _req(self, method, path, body=None):
        r = requests.request(method, self.server + path, json=body, timeout=TIMEOUT_S)
        try:
            status = r.status_code
            data = r.json()
        finally:
            r.close()
        if status >= 400:
            raise OSError("%d %s" % (status, data.get("error", "")))
        return data

    def open(self):
        """Join wifi, sync the clock and create a room."""
        connect()
        self.sync()
        data = self._req("POST", "/api/rhythm/rooms", {})
        self.code, self.token = data["code"], data["token"]

    def sync(self):
        """NTP-style: keep the sample with the shortest round trip."""
        best = None
        for _ in range(5):
            a = time.ticks_ms()
            t = self._req("GET", "/api/rhythm/time")["t"]
            rtt = time.ticks_diff(time.ticks_ms(), a)
            if best is None or rtt < best[0]:
                best = (rtt, time.ticks_add(a, rtt // 2), t)
        self.base_ms, self.base_server = best[1], best[2]

    def now(self):
        """Server time in ms, as an int."""
        return self.base_server + time.ticks_diff(time.ticks_ms(), self.base_ms)

    def command(self, action, **body):
        body["token"] = self.token
        self.listeners = self._req("POST", "/api/rhythm/rooms/%s/%s" % (self.code, action), body)["listeners"]

    def submit(self, run):
        """Upload a finished run. Returns the claim code, URL and QR rows."""
        run["room"], run["token"] = self.code, self.token
        return self._req("POST", "/api/rhythm/runs", run)
