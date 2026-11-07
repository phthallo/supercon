"""Link to the Quadrant site: web player commands, a clock shared with the server, and run upload."""

import json
import time

import network
import requests
import socket

WIFI_PATH = "/data/wifi.json"
JOIN_MS = 15000
# Each request opens a new TLS connection, which takes 1 to 1.5 s on the badge.
TIMEOUT_S = 10

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


def _read_json(s):
    """Read one HTTP/1.1 response with a Content-Length body and parse the body as JSON."""
    status = s.readline()
    if b" 200 " not in status:
        raise OSError("time: " + status.decode().strip())
    length = 0
    while True:
        line = s.readline()
        if line in (b"\r\n", b""):
            break
        k, _, v = line.partition(b":")
        if k.strip().lower() == b"content-length":
            length = int(v)
    body = b""
    while len(body) < length:
        chunk = s.read(length - len(body))
        if not chunk:
            raise OSError("time: short body")
        body += chunk
    return json.loads(body)


class Link:
    def __init__(self, server, key):
        self.server = server.rstrip("/")
        self.host = self.server.split("://", 1)[-1]
        self.auth = "Bearer " + key
        self.base_ms = 0
        self.base_server = 0
        self.listeners = 0

    def _req(self, method, path, body=None):
        # A new dict each time: requests writes Content-Length into the headers it is given.
        headers = {"Authorization": self.auth}
        r = requests.request(method, self.server + path, json=body, headers=headers, timeout=TIMEOUT_S)
        try:
            status = r.status_code
            data = r.json()
        finally:
            r.close()
        if status >= 400:
            raise OSError("%d %s" % (status, data.get("error", "")))
        return data

    def open(self):
        """Join wifi and sync the clock."""
        connect()
        self.sync()

    def sync(self):
        """NTP-style: keep the sample with the shortest round trip.

        All samples share one connection, so the TLS handshake (about 1 s on the badge)
        is not inside any round trip."""
        scheme, _, rest = self.server.partition("://")
        host, _, port = rest.partition("/")[0].partition(":")
        port = int(port) if port else (443 if scheme == "https" else 80)
        s = socket.socket()
        try:
            s.settimeout(TIMEOUT_S)
            s.connect(socket.getaddrinfo(host, port)[0][-1])
            if scheme == "https":
                import ssl

                s = ssl.wrap_socket(s, server_hostname=host)
            req = b"GET /api/rhythm/time HTTP/1.1\r\nHost: %s\r\n\r\n" % host.encode()
            best = None
            for _ in range(5):
                a = time.ticks_ms()
                s.write(req)
                t = _read_json(s)["t"]
                rtt = time.ticks_diff(time.ticks_ms(), a)
                if best is None or rtt < best[0]:
                    best = (rtt, time.ticks_add(a, rtt // 2), t)
        finally:
            s.close()
        self.base_ms, self.base_server = best[1], best[2]

    def now(self):
        """Server time in ms, as an int."""
        return self.base_server + time.ticks_diff(time.ticks_ms(), self.base_ms)

    def command(self, action, **body):
        self.listeners = self._req("POST", "/api/rhythm/player/" + action, body)["listeners"]

    def songs(self):
        return self._req("GET", "/api/rhythm/songs")["songs"]

    def song(self, sid):
        return self._req("GET", "/api/rhythm/songs/" + sid)

    def submit(self, run):
        """Upload a finished run. Returns the claim code, URL and QR rows."""
        return self._req("POST", "/api/rhythm/runs", run)
