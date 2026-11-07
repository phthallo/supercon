"""Quadrant: notes travel from the centre to the four corners. Hit them with + - A B."""

import asyncio as aio
import framebuf
import gc
import json
import math
import os
import time
from array import array

from apps.base_app import BaseApp
from lib import button
from lib.button import MenuMinus, MenuPlus, UserA, UserB
from lib.epd import display
from lib.led_bar import leds
from lib.qfont import CENTER, RIGHT, Font

W, H, CX, CY, HUB = 960, 672, 480, 336, 78
DATA = "/data/quadrant"
BEST_PATH = "/data/quadrant_best.json"
LAST_PATH = "/data/quadrant_last.json"
CFG_PATH = "/data/quadrant_cfg.json"
CALIB_NOTES = 24
# Taps on the first notes are left out of calibration while the player finds the beat.
CALIB_WARMUP = 4
DIFFS = ("easy", "normal", "hard")
DIFF_DOTS = (1, 3, 5)
# Index = button id from lib.button.next_press().
LANES = ((96, 96, "+"), (864, 96, "-"), (96, 576, "A"), (864, 576, "B"))

# Wide while there's no music to time against; tighten once the web player plays the audio.
PERFECT_MS, GREAT_MS, GOOD_MS = 80, 150, 220
JUDGE_NAMES = ("PERFECT", "GREAT", "GOOD", "MISS")
# osu!mania accuracy weights.
JUDGE_POINTS = (300, 200, 100, 0)
JUDGE_LEDS = ((0, 0, 8), (0, 6, 2), (6, 4, 0), (8, 0, 0))
TRAVEL_FRAMES = 4
# A delta frame shows on the panel 580-750 ms after update(), so each frame is sent this early.
LEAD_MS = 594
PAUSE_HOLD_MS = 150
# A hold counts if the button is let go no earlier than this before its end.
HOLD_RELEASE_MS = 150
LANE_BUTTONS = (MenuPlus, MenuMinus, UserA, UserB)

SELECT, PLAY, PAUSE, RESULTS = 0, 1, 2, 3
PAUSE_ITEMS = ("Resume", "Retry", "Song list")


def grade_for(counts):
    """osu!standard grades: the share of perfects sets the grade, and a miss caps it at A."""
    n = sum(counts)
    clean = counts[3] == 0

    def over(tenths):
        return counts[0] * 10 > tenths * n

    if over(9) and clean and counts[2] * 100 < n:
        return "S"
    if over(8) and clean or over(9):
        return "A"
    if over(7) and clean or over(8):
        return "B"
    if over(6):
        return "C"
    return "D"


def group(n):
    s = str(int(n))
    out = ""
    while len(s) > 3:
        out = " " + s[-3:] + out
        s = s[:-3]
    return s + out


def mmss(ms):
    s = max(0, int(ms)) // 1000
    return "%d:%02d" % (s // 60, s % 60)


def quad(fb, x1, y1, x2, y2, hw, c=1):
    dx, dy = x2 - x1, y2 - y1
    n = math.sqrt(dx * dx + dy * dy) or 1
    px, py = -dy / n * hw, dx / n * hw
    fb.poly(0, 0, array("h", (int(x1 + px), int(y1 + py), int(x2 + px), int(y2 + py),
                               int(x2 - px), int(y2 - py), int(x1 - px), int(y1 - py))), c, True)


def dashed(fb, x1, y1, x2, y2, hw, on=12, off=9):
    dx, dy = x2 - x1, y2 - y1
    n = math.sqrt(dx * dx + dy * dy)
    if n < 1:
        return
    ux, uy = dx / n, dy / n
    s = 0
    while s < n:
        e = min(n, s + on)
        quad(fb, x1 + ux * s, y1 + uy * s, x1 + ux * e, y1 + uy * e, hw)
        s += on + off


def stripes(fb, x, y, w, h, step=2):
    for xx in range(x, x + w, step):
        fb.vline(xx, y, h, 1)


def frame_rect(fb, x, y, w, h, t=2, c=1):
    for i in range(t):
        fb.rect(x + i, y + i, w - 2 * i, h - 2 * i, c)


def ring(fb, x, y, r, t=2, c=1):
    for i in range(t):
        fb.ellipse(x, y, r - i, r - i, c)


class Geometry:
    def __init__(self):
        self.start, self.end, self.normal = [], [], []
        for lx, ly, _ in LANES:
            dx, dy = lx - CX, ly - CY
            n = math.sqrt(dx * dx + dy * dy)
            sx, sy = CX + dx / n * HUB, CY + dy / n * HUB
            self.start.append((sx, sy))
            self.end.append((lx, ly))
            ex, ey = lx - sx, ly - sy
            m = math.sqrt(ex * ex + ey * ey)
            self.normal.append((-ey / m, ex / m))
        self.spikes = []
        for lx, ly, _ in LANES:
            polys = []
            for j in range(16):
                a, o = j * math.pi / 8, 0.1
                ln = 100 if j % 2 else 124
                polys.append(array("h", (
                    int(lx + math.cos(a - o) * 66), int(ly + math.sin(a - o) * 66),
                    int(lx + math.cos(a) * ln), int(ly + math.sin(a) * ln),
                    int(lx + math.cos(a + o) * 66), int(ly + math.sin(a + o) * 66))))
            self.spikes.append(polys)
        # Progress ring around the hub: 4 arcs of 12 quads.
        self.ring = []
        for k in range(4):
            a0 = -math.pi / 2 + k * math.pi / 2 + 0.13
            a1 = -math.pi / 2 + (k + 1) * math.pi / 2 - 0.13
            for s in range(12):
                b0 = a0 + (a1 - a0) * s / 12
                b1 = a0 + (a1 - a0) * (s + 1) / 12
                self.ring.append(array("h", (
                    int(CX + math.cos(b0) * 90), int(CY + math.sin(b0) * 90),
                    int(CX + math.cos(b1) * 90), int(CY + math.sin(b1) * 90),
                    int(CX + math.cos(b1) * 101), int(CY + math.sin(b1) * 101),
                    int(CX + math.cos(b0) * 101), int(CY + math.sin(b0) * 101))))

    def point(self, i, f):
        sx, sy = self.start[i]
        ex, ey = self.end[i]
        return sx + (ex - sx) * f, sy + (ey - sy) * f


class Run:
    def __init__(self, song, diff, chart):
        self.song = song
        self.diff = diff
        self.notes = chart
        self.judged = bytearray(len(chart))
        self.lane_notes = [[], [], [], []]
        for i, n in enumerate(chart):
            self.lane_notes[n[1]].append(i)
        # A hold note is judged twice: head and tail.
        self.total = len(chart) + sum(1 for n in chart if n[2])
        self.holding = [-1, -1, -1, -1]
        self.lane_ptr = [0, 0, 0, 0]
        self.draw_lo = 0
        self.counts = [0, 0, 0, 0]
        self.points = 0
        self.combo = 0
        self.max_combo = 0
        self.offsets = []
        self.recent = []
        self.fresh = []
        beat = 60000 / song["bpm"]
        self.period = math.ceil(700 / beat) * beat
        # Frames fall on the beat grid, which starts at offset_ms. The server places notes on these frames.
        self.phase = (song.get("offset_ms") or 0) % self.period
        last = max(n[0] + n[2] for n in chart) if chart else 0
        self.end_ms = last + 2 * self.period
        self.duration = song.get("duration_ms") or self.end_ms
        self.t0_us = 0
        self.next_frame = 0
        self.started = False
        self.resuming = False
        self.paused_ms = 0
        # Timing log for the last run: presses as [lane, song ms], frames as [T, sent ms, done ms].
        self.presses = []
        self.frames = []
        self.frame_open = False

    def score(self):
        n = self.total or 1
        return int(900000 * self.points / (300 * n) + 100000 * self.max_combo / n)

    def accuracy(self):
        judged = sum(self.counts)
        return self.points / (300 * judged) if judged else 1.0


def qr(fb, rows, x, y, size):
    """Draw a QR code from rows of "0"/"1", with its 4-module quiet zone, as large as fits in size."""
    n = len(rows) + 8
    s = size // n
    x += (size - s * n) // 2
    y += (size - s * n) // 2
    fb.fill_rect(x, y, s * n, s * n, 0)
    x += 4 * s
    y += 4 * s
    for j, row in enumerate(rows):
        i = 0
        while i < len(row):
            if row[i] == "1":
                k = i
                while k < len(row) and row[k] == "1":
                    k += 1
                fb.fill_rect(x + i * s, y + j * s, (k - i) * s, s, 1)
                i = k
            else:
                i += 1


def top_bar(fb, F, left, right=None):
    fb.fill_rect(0, 0, W, 56, 1)
    F["title"].draw(fb, left, 22, 29, 0)
    if right:
        F["body"].draw(fb, right, W - 22, 29, 0, RIGHT)


def keycap(fb, F, label, x, y, on_black):
    w = max(30, F["small"].width(label) + 16)
    fb.fill_rect(x, y - 15, w, 30, 0 if on_black else 1)
    frame_rect(fb, x, y - 15, w, 30, 2, 1 if on_black else 0)
    F["small"].draw(fb, label, x + w // 2, y, 1 if on_black else 0, CENTER)
    return w


def keys_row(fb, F, items, x, y, on_black):
    fg = 0 if on_black else 1
    for keys, label in items:
        for k in keys:
            x += keycap(fb, F, k, x, y, on_black) + 6
        x = F["small"].draw(fb, label, x + 4, y, fg) + 26
    return x


def hint_bar(fb, F, items, right=None):
    fb.fill_rect(0, 620, W, 52, 1)
    keys_row(fb, F, items, 22, 646, True)
    if right:
        F["small"].draw(fb, right, W - 22, 646, 0, RIGHT)


def cover(fb, x, y, s, name):
    fb.fill_rect(x, y, s, s, 1)
    seed = sum(ord(c) for c in name)
    cx = x + s * (45 + seed % 25) // 100
    cy = y + s * (30 + seed % 15) // 100
    r = min(cx - x, x + s - cx, cy - y) - 4
    k = 0
    while r > 2:
        fb.ellipse(cx, cy, r, r, k % 2 ^ 1, True)
        r -= max(3, s // 16)
        k += 1
    for k in range(5):
        yy = y + s * (70 + k * 6) // 100
        fb.fill_rect(x, yy, s, max(1, s * (1 + k) // 100), 0)
    frame_rect(fb, x, y, s, s, max(1, s // 64))


class Quadrant(BaseApp):
    owns_menu_buttons = True

    def __init__(self):
        super().__init__("Quadrant")
        self.F = None
        self.G = None
        self.state = SELECT
        self.dirty = True
        self.songs = []
        self.sel = 0
        self.diff = 1
        self.best = {}
        self.game = None
        self.pause_sel = 0
        self.result = None
        self._bg = None
        self._shown = None
        self.lead = LEAD_MS
        self.cfg = {}
        self.net = None
        self.net_err = None
        self.net_retry = 0
        self.claim = None

    def _launch(self):
        if self.F is None:
            self.F = {n: Font("%s/%s.qf" % (DATA, n)) for n in ("title", "big", "huge", "body", "small", "grade")}
            self.G = Geometry()
            self._bg = framebuf.FrameBuffer(bytearray(W * H // 8), W, H, framebuf.MONO_HLSB)
        self._load_songs()
        try:
            with open(BEST_PATH) as f:
                self.best = json.load(f)
        except (OSError, ValueError):
            self.best = {}
        try:
            with open(CFG_PATH) as f:
                cfg = json.load(f)
        except (OSError, ValueError):
            cfg = {}
        self.cfg = cfg
        try:
            self.lead = int(cfg["lead_ms"])
        except (KeyError, TypeError, ValueError):
            self.lead = LEAD_MS
        # "server" and "key" (the server's BADGE_KEY) turn on the web player.
        self.net_retry = time.ticks_ms()
        self.sel = min(self.sel, max(0, len(self.songs) - 1))
        self.state = SELECT
        self.game = None
        self.dirty = True
        self._shown = None
        display.session_max_frames = 20
        button.clear_presses()

    def _load_songs(self):
        self.songs = []
        try:
            names = sorted(n for n in os.listdir(DATA + "/songs") if n.endswith(".json"))
        except OSError:
            names = []
        for n in names:
            path = "%s/songs/%s" % (DATA, n)
            with open(path) as f:
                song = json.load(f)
            charts = song.pop("charts")
            song["path"] = path
            song["id"] = n[:-5]
            song["counts"] = {d: len(charts.get(d, ())) for d in DIFFS}
            # Judgments per chart as the server counts them: a hold is judged at its start and end.
            song["notes"] = {d: sum(2 if len(x) > 2 else 1 for x in c) for d, c in charts.items()}
            self.songs.append(song)
            del charts
            gc.collect()
        self.songs.append({"title": "Calibrate", "artist": "Timing", "bpm": 100, "calib": True,
                           "duration_ms": (CALIB_NOTES + 3) * 1200,
                           "counts": {d: CALIB_NOTES for d in DIFFS}})

    def _pull_songs(self):
        """Download songs the server has and the badge lacks or holds an older chart of.

        Songs removed from the server stay on the badge."""
        local = {s["id"]: s for s in self.songs if "id" in s}
        changed = False
        for s in self.net.songs():
            sid = s.get("id", "")
            if not sid or not all(c.isdigit() or "a" <= c <= "z" or c == "-" for c in sid):
                continue
            have = local.get(sid)
            if have and all(have.get(k) == s.get(k) for k in ("title", "artist", "bpm", "offset_ms", "notes")):
                continue
            full = self.net.song(sid)
            keep = {k: full[k] for k in ("title", "artist", "bpm", "offset_ms", "duration_ms", "charts") if k in full}
            tmp = "%s/songs/%s.tmp" % (DATA, sid)
            with open(tmp, "w") as f:
                json.dump(keep, f)
            os.rename(tmp, "%s/songs/%s.json" % (DATA, sid))
            print("quadrant: pulled", sid)
            changed = True
            del full, keep
            gc.collect()
        if changed:
            cur = self.songs[self.sel].get("id") if self.songs else None
            self._load_songs()
            for i, s in enumerate(self.songs):
                if s.get("id") == cur:
                    self.sel = i

    def _best(self, song, diff):
        return self.best.get("%s/%s" % (song["title"], diff))

    async def arun(self):
        if self.launched:
            self._launch()
        if self.state == PLAY:
            await self._play()
            return
        while True:
            p = button.next_press()
            if p is None:
                break
            self._on_press(p[0])
        if self.dirty and display.is_done_updating():
            self.dirty = False
            # A full refresh on every screen change clears the previous screen's ghosting.
            if self.state != self._shown:
                display.request_clean()
                self._shown = self.state
            if self.state == SELECT:
                self._draw_select()
            elif self.state == PAUSE:
                self._draw_pause()
            elif self.state == RESULTS:
                self._draw_results()
            display.update()
        elif self.state == SELECT and not self.dirty and display.is_done_updating():
            self._connect()
        await aio.sleep_ms(20)

    def _online(self):
        return bool(self.cfg.get("server") and self.cfg.get("key"))

    def _connect(self):
        """Join wifi and sync with the server once the song list is showing. Blocks for up to about 20 s."""
        if not self._online() or self.net or time.ticks_diff(time.ticks_ms(), self.net_retry) < 0:
            return
        try:
            from lib import qnet

            link = qnet.Link(self.cfg["server"], self.cfg["key"])
            link.open()
            self.net, self.net_err = link, None
        except Exception as e:
            print("quadrant: offline", e)
            self.net_err = "OFFLINE"
            self.net_retry = time.ticks_add(time.ticks_ms(), 120000)
        if self.net:
            try:
                self._pull_songs()
            except Exception as e:
                print("quadrant: song download", e)
        self.dirty = True

    def _player(self, action, **body):
        """Send a web player command. Never raises, since the game plays on without the web player."""
        if not self.net or self.game.song.get("calib"):
            return
        try:
            if action in ("start", "resume"):
                # Server time at which song position 0 plays, from the game clock's origin.
                body["start_at"] = self.net.now() + time.ticks_diff(self.game.t0_us, time.ticks_us()) // 1000
            self.net.command(action, **body)
        except Exception as e:
            print("quadrant: player", action, e)

    def _on_press(self, b):
        if self.state == SELECT:
            if not self.songs:
                return
            if b == 0:
                self.sel = (self.sel + 1) % len(self.songs)
            elif b == 1:
                self.sel = (self.sel - 1) % len(self.songs)
            elif b == 2:
                self._start(self.songs[self.sel], DIFFS[self.diff])
                return
            elif b == 3:
                self.diff = (self.diff + 1) % len(DIFFS)
            self.dirty = True
        elif self.state == PAUSE:
            if b == 0:
                self.pause_sel = (self.pause_sel + 1) % len(PAUSE_ITEMS)
            elif b == 1:
                self.pause_sel = (self.pause_sel - 1) % len(PAUSE_ITEMS)
            elif b == 2:
                if self.pause_sel == 0:
                    self._resume()
                elif self.pause_sel == 1:
                    self._start(self.game.song, self.game.diff)
                else:
                    self._player("stop")
                    self.state = SELECT
                    self.game = None
                return
            self.dirty = True
        elif self.state == RESULTS:
            if b == 0:
                self._start(self.game.song, self.game.diff)
            elif b == 2 and self.game.song.get("calib") and self.result[1] is not None:
                self._save_lead(self.result[2])
                self.state = SELECT
                self.game = None
                self.dirty = True
            elif b == 3:
                self.state = SELECT
                self.game = None
                self.dirty = True

    def _save_lead(self, lead):
        self.lead = lead
        try:
            self.cfg["lead_ms"] = lead
            with open(CFG_PATH, "w") as f:
                json.dump(self.cfg, f)
        except OSError as e:
            print("quadrant: lead not saved", e)

    def _start(self, song, diff):
        self.game = None
        gc.collect()
        if song.get("calib"):
            # One note per frame, clockwise round the corners.
            chart = [(2400 + k * 1200, (0, 1, 3, 2)[k % 4], 0) for k in range(CALIB_NOTES)]
        else:
            with open(song["path"]) as f:
                chart = json.load(f)["charts"].get(diff, [])
            gc.collect()
            # Entries are [ms, lane] for a tap or [ms, lane, dur_ms] for a hold.
            chart = [(int(n[0]), int(n[1]), int(n[2]) if len(n) > 2 else 0) for n in chart if 0 <= n[1] <= 3]
            chart.sort()
        self.game = Run(song, diff, chart)
        self._build_bg(self.game)
        self.state = PLAY

    def _clock(self, r, song_ms):
        """Set the clock so song time is song_ms - lead now, and the next frame is due at once."""
        r.t0_us = time.ticks_add(time.ticks_us(), int((self.lead - song_ms) * 1000))
        r.next_frame = math.ceil((song_ms - r.phase) / r.period)

    def _song_ms(self, r, us):
        return time.ticks_diff(us, r.t0_us) / 1000

    async def _play(self):
        r = self.game
        P = r.period
        display.session_max_frames = 1000
        if not r.started or r.resuming:
            resume = r.started
            if resume:
                s0 = r.paused_ms - 2 * P
            else:
                first = r.notes[0][0] if r.notes else 0
                s0 = min(0, first - TRAVEL_FRAMES * P) - P
                if self.net and not r.song.get("calib"):
                    # Gives the web player time to fetch and decode the audio.
                    s0 = min(s0, -3000)
            r.started = True
            r.resuming = False
            self._shown = PLAY
            # A refresh still running would mark the panel clean after request_clean() below.
            while not display.is_done_updating():
                await aio.sleep_ms(20)
            display.red.fill(0)
            self._draw_game(r, r.phase + math.ceil((s0 - r.phase) / P) * P)
            display.request_clean()
            display.update()
            await aio.sleep_ms(50)
            while not display.is_done_updating():
                await aio.sleep_ms(20)
            button.clear_presses()
            if self.net and not r.song.get("calib"):
                try:
                    # The badge clock drifts from the server's by up to ~0.1 s in half an hour.
                    self.net.sync()
                except Exception as e:
                    print("quadrant: sync", e)
            self._clock(r, s0)
            if resume:
                self._player("resume")
            else:
                self._player("start", song=r.song["id"], diff=r.diff)
        while True:
            T = r.phase + r.next_frame * P
            if T > r.end_ms:
                break
            due = time.ticks_add(r.t0_us, int((T - self.lead) * 1000))
            while True:
                if self._poll(r):
                    self._pause(r)
                    return
                left = time.ticks_diff(due, time.ticks_us())
                done = display.is_done_updating()
                if done and r.frame_open:
                    r.frames[-1].append(round(self._song_ms(r, time.ticks_us())))
                    r.frame_open = False
                if left <= 0 and done:
                    break
                await aio.sleep_ms(min(10, left // 1000) if left > 2000 else 2)
            self._draw_game(r, T)
            r.frames.append([round(T), round(self._song_ms(r, time.ticks_us()))])
            display.update()
            r.frame_open = True
            r.next_frame += 1
        display.session_max_frames = 20
        leds.fill((0, 0, 0))
        self._finish(r)

    def _poll(self, r):
        """Judge queued presses and sweep missed notes. True when + and - are held to pause."""
        while True:
            p = button.next_press()
            if p is None:
                break
            if p[0] <= 3:
                t = self._song_ms(r, p[1])
                r.presses.append([p[0], round(t)])
                lane = p[0]
                if r.song.get("calib") and r.notes:
                    lane = min(r.notes, key=lambda n: abs(n[0] - t))[1]
                self._hit(r, lane, t)
        now = self._song_ms(r, time.ticks_us())
        notes, judged = r.notes, r.judged
        for lane in range(4):
            idx, ptr = r.lane_notes[lane], r.lane_ptr[lane]
            while ptr < len(idx) and (judged[idx[ptr]] or notes[idx[ptr]][0] < now - GOOD_MS):
                if not judged[idx[ptr]]:
                    self._judge(r, idx[ptr], 3, 0)
                ptr += 1
            r.lane_ptr[lane] = ptr
            i = r.holding[lane]
            if i >= 0:
                end = notes[i][0] + notes[i][2]
                if now >= end:
                    self._end_hold(r, lane, 0)
                elif not self._held(lane):
                    self._end_hold(r, lane, 0 if now >= end - HOLD_RELEASE_MS else 3)
        # A hold on + or - can't double as the pause chord.
        return (r.holding[0] < 0 and r.holding[1] < 0
                and MenuPlus.pressed_ms() >= PAUSE_HOLD_MS and MenuMinus.pressed_ms() >= PAUSE_HOLD_MS)

    def _held(self, lane):
        return LANE_BUTTONS[lane].pin.value()

    def _end_hold(self, r, lane, kind):
        r.holding[lane] = -1
        self._score(r, lane, kind, None)

    def _hit(self, r, lane, t):
        idx = r.lane_notes[lane]
        ptr = r.lane_ptr[lane]
        while ptr < len(idx):
            i = idx[ptr]
            if r.judged[i]:
                ptr += 1
                continue
            off = t - r.notes[i][0]
            if off < -GOOD_MS:
                break
            if off > GOOD_MS:
                self._judge(r, i, 3, 0)
                ptr += 1
                continue
            a = abs(off)
            self._judge(r, i, 0 if a <= PERFECT_MS else 1 if a <= GREAT_MS else 2, int(off))
            ptr += 1
            break
        r.lane_ptr[lane] = ptr

    def _judge(self, r, i, kind, off):
        r.judged[i] = kind + 1
        _, lane, dur = r.notes[i]
        self._score(r, lane, kind, off if kind < 3 else None)
        if dur:
            if kind == 3:
                self._score(r, lane, 3, None)
            else:
                r.holding[lane] = i

    def _score(self, r, lane, kind, off):
        """Count one judgment. off is None for misses and hold tails, which have no timing."""
        r.counts[kind] += 1
        r.points += JUDGE_POINTS[kind]
        if kind == 3:
            r.combo = 0
        else:
            r.combo += 1
            r.max_combo = max(r.max_combo, r.combo)
        if off is not None:
            r.offsets.append(off)
            r.recent.append(off)
            if len(r.recent) > 6:
                r.recent.pop(0)
        r.fresh.append((lane, kind, off or 0))
        leds.fill(JUDGE_LEDS[kind])

    def _pause(self, r):
        r.paused_ms = self._song_ms(r, time.ticks_us())
        r.frame_open = False
        for lane in range(4):
            if r.holding[lane] >= 0:
                self._end_hold(r, lane, 3)
        # The menu opens after a further 500 ms hold, so a hold to pause doesn't also leave the app.
        now = time.ticks_ms()
        MenuPlus._press_onset_ms = now
        MenuMinus._press_onset_ms = now
        button.clear_presses()
        display.session_max_frames = 20
        leds.fill((0, 0, 0))
        self.pause_sel = 0
        self.state = PAUSE
        self.dirty = True
        self._player("pause", pos_ms=max(1, int(r.paused_ms)))

    def _resume(self):
        self.game.resuming = True
        button.clear_presses()
        self.state = PLAY

    def _calib_result(self, r):
        offs = []
        start = r.notes[CALIB_WARMUP][0] - 300
        for _, t in r.presses:
            if t < start:
                continue
            h = min(r.notes, key=lambda n: abs(n[0] - t))[0]
            if abs(t - h) <= 300:
                offs.append(t - h)
        if len(offs) < 8:
            return ("calib", None, self.lead, len(offs))
        offs.sort()
        med = offs[len(offs) // 2]
        # Early taps mean frames show early, so send them later by the same amount.
        return ("calib", med, max(300, min(900, self.lead + med)), len(offs))

    def _finish(self, r):
        if r.song.get("calib"):
            self._write_log(r)
            self.result = self._calib_result(r)
            self.state = RESULTS
            self.dirty = True
            return
        acc = r.accuracy() if sum(r.counts) else 0.0
        score = r.score()
        key = "%s/%s" % (r.song["title"], r.diff)
        prev = self.best.get(key)
        g = grade_for(r.counts)
        new_best = prev is None or score > prev[0]
        if new_best:
            self.best[key] = [score, round(acc * 10000) / 100, g]
            try:
                with open(BEST_PATH, "w") as f:
                    json.dump(self.best, f)
            except OSError as e:
                print("quadrant: best not saved", e)
        self._write_log(r)
        self.result = (score, acc, g, new_best, prev)
        self.claim = self._submit(r, score, acc, g) if self.net else None
        self.state = RESULTS
        self.dirty = True

    def _submit(self, r, score, acc, g):
        """Upload the run. Returns the server's claim (code, url, qr) or an error string."""
        gc.collect()
        try:
            return self.net.submit({"song": r.song["id"], "diff": r.diff, "score": score, "acc": acc, "grade": g,
                                    "counts": r.counts, "max_combo": r.max_combo, "notes": r.total})
        except Exception as e:
            print("quadrant: run not uploaded", e)
            return "NOT UPLOADED"

    def _write_log(self, r):
        try:
            with open(LAST_PATH, "w") as f:
                json.dump({"song": r.song["title"], "diff": r.diff, "lead_ms": self.lead,
                           "notes": r.notes, "presses": r.presses, "frames": r.frames}, f)
        except OSError as e:
            print("quadrant: timing log not saved", e)

    def _build_bg(self, r):
        fb, F, G = self._bg, self.F, self.G
        fb.fill(0)
        for rad in (190, 280, 380, 490):
            steps = int(2 * math.pi * rad / 10)
            for k in range(steps):
                a = 2 * math.pi * k / steps
                fb.fill_rect(int(CX + math.cos(a) * rad), int(CY + math.sin(a) * rad), 2, 2, 1)
        for i in range(4):
            (sx, sy), (ex, ey), (px, py) = G.start[i], G.end[i], G.normal[i]
            w0, w1 = 20, 66
            pts = (sx + px * w0, sy + py * w0, ex + px * w1, ey + py * w1,
                   ex - px * w1, ey - py * w1, sx - px * w0, sy - py * w0)
            fb.poly(0, 0, array("h", [int(v) for v in pts]), 0, True)
            for o in ((0, 0), (1, 0), (0, 1), (1, 1)):
                fb.poly(o[0], o[1], array("h", [int(v) for v in pts]), 1)
            for f in (0.25, 0.5, 0.75):
                x, y = G.point(i, f)
                w = w0 + (w1 - w0) * f
                quad(fb, x + px * w, y + py * w, x - px * w, y - py * w, 1)
            self._receptor(fb, i, False)
        ring(fb, CX, CY, 96, 1)
        ring(fb, CX, CY, 95, 1)
        tally_y = 280
        for k, name in enumerate(JUDGE_NAMES):
            y = tally_y + k * 32
            if k == 0:
                fb.fill_rect(24, y - 8, 16, 16, 1)
            elif k < 3:
                frame_rect(fb, 24, y - 8, 16, 16, 1)
                stripes(fb, 24, y - 8, 16, 16, k + 1)
            else:
                frame_rect(fb, 24, y - 8, 16, 16, 2)
            F["small"].draw(fb, name, 50, y)
        F["small"].draw(fb, "EARLY", 918, 236, 1, CENTER, 1)
        F["small"].draw(fb, "LATE", 918, 438, 1, CENTER, 1)
        fb.fill_rect(917, 252, 2, 170, 1)
        fb.fill_rect(900, 336, 36, 3, 1)
        line = "%s  %s" % (r.song["title"], r.diff.upper())
        F["body"].draw(fb, line, CX, 598, 1, CENTER)
        frame_rect(fb, 332, 623, 296, 14, 2)
        F["small"].draw(fb, mmss(r.duration), 638, 630)

    def _glyph(self, fb, i, c):
        x, y, k = LANES[i]
        if k in "+-":
            fb.fill_rect(x - 16, y - 5, 32, 10, c)
            if k == "+":
                fb.fill_rect(x - 5, y - 16, 10, 32, c)
        else:
            self.F["title"].draw(fb, k, x, y, c, CENTER)

    def _receptor(self, fb, i, hit):
        x, y, _ = LANES[i]
        if hit:
            ring(fb, x, y, 86, 3)
            for p in self.G.spikes[i]:
                fb.poly(0, 0, p, 1, True)
            fb.ellipse(x, y, 58, 58, 1, True)
            ring(fb, x, y, 46, 2, 0)
            self._glyph(fb, i, 0)
        else:
            fb.ellipse(x, y, 58, 58, 1, True)
            fb.ellipse(x, y, 50, 50, 0, True)
            ring(fb, x, y, 44, 2)
            self._glyph(fb, i, 1)

    def _hold_body(self, fb, lane, f_head, f_tail):
        if f_head - f_tail < 0.01:
            return
        x1, y1 = self.G.point(lane, f_head)
        x2, y2 = self.G.point(lane, f_tail)
        quad(fb, x1, y1, x2, y2, 14, 1)
        quad(fb, x1, y1, x2, y2, 8, 0)
        rad = int(8 + 10 * f_tail)
        fb.ellipse(int(x2), int(y2), rad, rad, 1, True)

    def _draw_game(self, r, T):
        fb, F, G = display.black, self.F, self.G
        fb.blit(self._bg, 0, 0)
        travel = TRAVEL_FRAMES * r.period
        notes, judged = r.notes, r.judged
        while r.draw_lo < len(notes) and notes[r.draw_lo][0] < T - GOOD_MS:
            r.draw_lo += 1
        for lane in range(4):
            i = r.holding[lane]
            if i >= 0:
                h, _, dur = notes[i]
                self._hold_body(fb, lane, 1.0, max(0.0, 1 - (h + dur - T) / travel))
                self._receptor(fb, lane, True)
        visible = []
        j = r.draw_lo
        while j < len(notes) and notes[j][0] <= T + travel:
            if not judged[j]:
                h, lane, dur = notes[j]
                f = min(1.0, 1 - (h - T) / travel)
                if dur:
                    self._hold_body(fb, lane, f, max(0.0, 1 - (h + dur - T) / travel))
                x, y = G.point(lane, f)
                visible.append((h, int(x), int(y), int(12 + 22 * f)))
            j += 1
        for k in range(1, len(visible)):
            if visible[k][0] == visible[k - 1][0]:
                dashed(fb, visible[k - 1][1], visible[k - 1][2], visible[k][1], visible[k][2], 2)
        for _, x, y, rad in visible:
            fb.ellipse(x, y, rad, rad, 1, True)
            fb.ellipse(x, y, rad * 64 // 100, rad * 64 // 100, 0, True)
            fb.ellipse(x, y, rad * 34 // 100, rad * 34 // 100, 1, True)

        for lane, kind, _ in r.fresh:
            if kind < 3:
                self._receptor(fb, lane, True)
        if not r.fresh:
            leds.fill((0, 0, 0))
        r.fresh = []

        prog = max(0.0, min(1.0, T / r.duration)) if r.duration else 0
        for q in G.ring[: int(prog * len(G.ring))]:
            fb.poly(0, 0, q, 1, True)
        fb.ellipse(CX, CY, HUB, HUB, 1, True)
        ring(fb, CX, CY, HUB - 8, 2, 0)
        F["big"].draw(fb, str(r.combo), CX, CY - 8, 0, CENTER)
        F["small"].draw(fb, "COMBO", CX, CY + 30, 0, CENTER, 3)

        F["big"].draw(fb, group(r.score()), CX, 46, 1, CENTER)
        F["body"].draw(fb, "%.2f%%" % (r.accuracy() * 100), CX, 90, 1, CENTER)
        for k in range(4):
            F["small"].draw(fb, str(r.counts[k]), 200, 280 + k * 32, 1, RIGHT)

        n = len(r.recent)
        for k, ms in enumerate(r.recent):
            y = 337 + max(-70, min(70, ms * 70 // GOOD_MS))
            w = 8 + 20 * (k + 1) // n
            fb.fill_rect(918 - w // 2, y - 2, w, 4, 1)
            if k == n - 1:
                fb.poly(0, 0, array("h", (944, y - 7, 944, y + 7, 936, y)), 1, True)

        F["small"].draw(fb, mmss(max(0, T)), 322, 630, 1, RIGHT)
        fb.fill_rect(335, 626, int(290 * prog), 8, 1)

    def _draw_select(self):
        fb, F = display.black, self.F
        fb.fill(0)
        display.red.fill(0)
        link = None
        if self.net:
            link = self.net.host + "/play"
        elif self._online():
            link = self.net_err or "CONNECTING"
        top_bar(fb, F, "QUADRANT", link)
        hint = [(("-", "+"), "browse"), (("A",), "play"), (("B",), "difficulty")]
        if not self.songs:
            F["body"].draw(fb, "No songs in " + DATA + "/songs", CX, CY, 1, CENTER)
            hint_bar(fb, F, [], "hold - + for menu")
            return
        song = self.songs[self.sel]
        diff = DIFFS[self.diff]
        cover(fb, 36, 78, 196, song["title"])
        words = song["title"].upper().split()
        lines, cur = [], ""
        for w in words:
            if cur and F["title"].width(cur + " " + w) > 188:
                lines.append(cur)
                cur = w
            else:
                cur = (cur + " " + w).strip()
        lines.append(cur)
        y = 98
        for ln in lines[:2]:
            F["title"].draw(fb, ln, 252, y)
            y += 32
        F["body"].draw(fb, song.get("artist", ""), 252, 164)
        if song.get("calib"):
            hint = [(("-", "+"), "browse"), (("A",), "start")]
            F["small"].draw(fb, "%d notes, 1.2 s apart" % CALIB_NOTES, 252, 194)
            frame_rect(fb, 36, 290, 404, 316, 2)
            F["small"].draw(fb, "LEAD", 52, 312, 1, 0, 2)
            F["huge"].draw(fb, "%d ms" % self.lead, 238, 390, 1, CENTER)
            F["body"].draw(fb, "Press any button", 238, 480, 1, CENTER)
            F["body"].draw(fb, "as each note hits", 238, 512, 1, CENTER)
        else:
            self._draw_song_info(fb, F, song, diff)
        self._draw_song_list(fb, F, diff)
        hint_bar(fb, F, hint, "hold - + for menu")

    def _draw_song_info(self, fb, F, song, diff):
        F["small"].draw(fb, "%d BPM   %s" % (song["bpm"], mmss(song.get("duration_ms", 0))), 252, 194)
        F["small"].draw(fb, "%d notes" % song["counts"][diff], 252, 216)

        for k, name in enumerate(DIFFS):
            x = 36 + k * 138
            if k == self.diff:
                fb.fill_rect(x, 290, 128, 40, 1)
            else:
                frame_rect(fb, x, 290, 128, 40, 2)
            F["body"].draw(fb, name.upper(), x + 64, 310, 0 if k == self.diff else 1, CENTER, 1)
            for s in range(5):
                if s < DIFF_DOTS[k]:
                    fb.ellipse(x + 40 + s * 12, 344, 4, 4, 1, True)
                else:
                    fb.ellipse(x + 40 + s * 12, 344, 4, 4, 1)

        frame_rect(fb, 36, 362, 404, 244, 2)
        F["small"].draw(fb, "BEST", 52, 384, 1, 0, 2)
        for k, name in enumerate(DIFFS):
            y = 432 + k * 58
            sel = k == self.diff
            fg = 0 if sel else 1
            if sel:
                fb.fill_rect(40, y - 26, 396, 52, 1)
            F["small"].draw(fb, name.upper(), 56, y, fg, 0, 1)
            b = self._best(song, name)
            if b:
                F["title"].draw(fb, b[2], 190, y, fg, CENTER)
                F["body"].draw(fb, group(b[0]), 340, y, fg, RIGHT)
                F["small"].draw(fb, "%.1f%%" % b[1], 424, y, fg, RIGHT)
            else:
                F["small"].draw(fb, "-", 190, y, fg, CENTER)

    def _draw_song_list(self, fb, F, diff):
        for k, s in enumerate(self.songs):
            off = k - self.sel
            d = abs(off)
            if d > 3:
                continue
            x, y = 470 + d * d * 13, CY + off * 82 - 36
            w = 938 - x
            sel = off == 0
            fg = 0 if sel else 1
            if sel:
                stripes(fb, x + 8, y + 8, w, 74)
                fb.fill_rect(x, y, w, 74, 1)
                fb.poly(0, 0, array("h", (x - 20, y + 22, x - 20, y + 52, x - 6, y + 37)), 1, True)
            else:
                fb.fill_rect(x, y, w, 74, 0)
                frame_rect(fb, x, y, w, 74, 3 if d < 3 else 1)
            cover(fb, x + 12, y + 13, 48, s["title"])
            F["body"].draw(fb, s["title"], x + 74, y + 26, fg)
            F["small"].draw(fb, "%s   %d BPM" % (s.get("artist", ""), s["bpm"]), x + 74, y + 53, fg)
            if s.get("calib"):
                continue
            b = self._best(s, diff)
            gx = x + w - 48
            if b:
                frame_rect(fb, gx, y + 19, 36, 36, 2, fg)
                F["title"].draw(fb, b[2], gx + 18, y + 37, fg, CENTER)
            else:
                F["small"].draw(fb, "NEW", gx + 18, y + 37, fg, CENTER, 1)

    def _draw_pause(self):
        fb, F, r = display.black, self.F, self.game
        stripes(fb, 282, 152, 420, 400)
        fb.fill_rect(270, 140, 420, 400, 0)
        frame_rect(fb, 270, 140, 420, 400, 6)
        F["huge"].draw(fb, "PAUSED", 480, 200, 1, CENTER)
        F["small"].draw(fb, "%s / %s" % (mmss(r.paused_ms), mmss(r.duration)), 480, 244, 1, CENTER)
        for k, label in enumerate(PAUSE_ITEMS):
            y = 286 + k * 52
            fb.fill_rect(306, y, 348, 44, 0)
            if k == self.pause_sel:
                fb.fill_rect(306, y, 348, 44, 1)
                fb.poly(0, 0, array("h", (322, y + 12, 322, y + 32, 338, y + 22)), 0, True)
                F["body"].draw(fb, label, 352, y + 22, 0)
            else:
                frame_rect(fb, 306, y, 348, 44, 2)
                F["body"].draw(fb, label, 352, y + 22, 1)
        keys_row(fb, F, [(("-", "+"), "move"), (("A",), "select")], 306, 474, False)
        F["small"].draw(fb, "hold - + for menu", 306, 512)

    def _draw_calib_results(self):
        fb, F = display.black, self.F
        _, med, lead, n = self.result
        fb.fill(0)
        display.red.fill(0)
        top_bar(fb, F, "CALIBRATION")
        if med is None:
            F["title"].draw(fb, "Not enough taps", CX, 300, 1, CENTER)
            F["body"].draw(fb, "%d of %d counted, 8 needed" % (n, CALIB_NOTES - CALIB_WARMUP), CX, 350, 1, CENTER)
            hint_bar(fb, F, [(("B",), "song list"), (("+",), "retry")])
            return
        F["huge"].draw(fb, "%+d ms" % med, CX, 220, 1, CENTER)
        F["body"].draw(fb, "early" if med < 0 else "late" if med > 0 else "on time", CX, 280, 1, CENTER)
        F["small"].draw(fb, "median of %d taps" % n, CX, 314, 1, CENTER)
        frame_rect(fb, 280, 380, 400, 120, 2)
        F["small"].draw(fb, "LEAD", 296, 402, 1, 0, 2)
        F["big"].draw(fb, "%d  >  %d ms" % (self.lead, lead), CX, 452, 1, CENTER)
        hint_bar(fb, F, [(("A",), "save"), (("B",), "song list"), (("+",), "retry")])

    def _draw_results(self):
        fb, F, r = display.black, self.F, self.game
        if r.song.get("calib"):
            self._draw_calib_results()
            return
        score, acc, g, new_best, prev = self.result
        n = r.total or 1
        fb.fill(0)
        display.red.fill(0)
        gx, gy = 220, 300
        for j in range(12):
            a, o = j * math.pi / 6, 0.07
            pts = []
            for rad, b in ((150, a - 0.04), (230, a - o), (230, a + o), (150, a + 0.04)):
                pts += (int(gx + math.cos(b) * rad), int(gy + math.sin(b) * rad))
            fb.poly(0, 0, array("h", pts), 1)
        F["grade"].draw(fb, g, gx + 8, gy + 8, 1, CENTER)
        fb.fill_rect(0, 430, 450, 190, 0)
        top_bar(fb, F, "RESULTS", "%s  %s" % (r.song["title"], r.diff.upper()))
        if new_best and prev:
            fb.fill_rect(276, 96, 148, 44, 0)
            frame_rect(fb, 276, 96, 148, 44, 4)
            F["title"].draw(fb, "NEW BEST", 350, 118, 1, CENTER)
        F["big"].draw(fb, "%.2f%%" % (acc * 100), 220, 488, 1, CENTER)
        F["small"].draw(fb, "ACCURACY", 220, 526, 1, CENTER, 3)

        F["small"].draw(fb, "SCORE", 466, 88, 1, 0, 3)
        F["huge"].draw(fb, group(score), 466, 130)
        boxes = (("MAX COMBO", "%d / %d" % (r.max_combo, n)),
                 ("PREVIOUS BEST", group(prev[0]) if prev else "-"))
        for k, (label, value) in enumerate(boxes):
            x = 466 + k * 236
            frame_rect(fb, x, 168, 224, 62, 2)
            F["small"].draw(fb, label, x + 14, 186, 1, 0, 2)
            F["body"].draw(fb, value, x + 14, 212)
        for k, name in enumerate(JUDGE_NAMES):
            y = 262 + k * 32
            c = r.counts[k]
            F["small"].draw(fb, name, 466, y)
            bw = max(4, c * 290 // n)
            if k == 0:
                fb.fill_rect(570, y - 10, bw, 20, 1)
            elif k < 3:
                stripes(fb, 570, y - 10, bw, 20, k + 1)
            else:
                frame_rect(fb, 570, y - 10, 8, 20, 2)
            F["small"].draw(fb, str(c), 934, y, 1, RIGHT)

        frame_rect(fb, 466, 400, 470, 206, 2)
        F["small"].draw(fb, "TIMING", 482, 420, 1, 0, 2)
        claim = self.claim if isinstance(self.claim, dict) else None
        if claim and claim.get("qr"):
            hist_w = 245
            qr(fb, claim["qr"], 734, 402, 200)
        else:
            hist_w = 445
        right = 475 + hist_w
        if r.offsets:
            avg = sum(r.offsets) / len(r.offsets)
            F["small"].draw(fb, "avg %+d ms" % round(avg), right, 420, 1, RIGHT)
        nb = 2 * GOOD_MS // 10 + 1
        bw = hist_w // nb
        bins = [0] * nb
        for off in r.offsets:
            bins[max(0, min(nb - 1, (off + GOOD_MS) // 10))] += 1
        top = max(bins) or 1
        for i, c in enumerate(bins):
            h = c * 130 // top + 2
            x = 481 + i * bw
            if abs(-GOOD_MS + 5 + i * 10) <= PERFECT_MS:
                fb.fill_rect(x, 590 - h, bw - 1, h, 1)
            else:
                stripes(fb, x, 590 - h, bw - 1, h)
        for yy in range(450, 594, 10):
            fb.vline(481 + nb * bw // 2, yy, 5, 1)
        F["small"].draw(fb, "early", 482, 442)
        F["small"].draw(fb, "late", right, 442, 1, RIGHT)
        note = None
        if claim:
            note = "scan to save   " + claim["code"]
        elif self.claim:
            note = self.claim
        hint_bar(fb, F, [(("B",), "song list"), (("+",), "retry")], note)
