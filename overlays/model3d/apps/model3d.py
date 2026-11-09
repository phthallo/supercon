"""Wireframe viewer for a Wavefront OBJ mesh. The README lists the controls and mesh limits."""

import array
import asyncio as aio
import gc
import math

from apps.base_app import BaseApp
from lib import button
from lib.epd import display
from lib.led_bar import leds

MESH_PATH = "/data/model3d/robot.obj"
MAX_VERTS = 512
MAX_EDGES = 1024

W, H, CX, CY = 960, 672, 480, 336

# The view is two indices into these tables, so a full turn returns to exactly the same frame.
STEPS = 24
Q = 14
SIN = [round(math.sin(2 * math.pi * i / STEPS) * (1 << Q)) for i in range(STEPS)]
COS = [SIN[(i + STEPS // 4) % STEPS] for i in range(STEPS)]

# Vertices are scaled to lie within RADIUS of the origin. A sphere of that radius seen from
# DISTANCE projects to FOCAL * RADIUS / sqrt(DISTANCE^2 - RADIUS^2) = 320 px, which fits the
# panel height at every rotation.
RADIUS = 1024
DISTANCE = 3 * RADIUS
FOCAL = 905

START_YAW = 2
START_PITCH = 1

# (yaw, pitch) change, by button id from lib.button.next_press().
PRESS_STEP = ((-1, 0), (1, 0), (0, 1), (0, -1))

FULL_REFRESH_LEDS = (0, 0, 1)


def _vertex_index(token, count):
    i = int(token.split("/")[0])
    i = i - 1 if i > 0 else count + i
    if not 0 <= i < count:
        raise ValueError("bad vertex index")
    return i


def load_obj(path):
    """Read v, f and l lines. Returns (array of x,y,z in model units, array of edge index pairs)."""
    points = []
    seen = set()
    with open(path) as f:
        for line in f:
            fields = line.split()
            if not fields:
                continue
            kind = fields[0]
            if kind == "v":
                if len(points) == MAX_VERTS:
                    raise ValueError("too many vertices")
                points.append((float(fields[1]), float(fields[2]), float(fields[3])))
            elif kind == "f" or kind == "l":
                ids = [_vertex_index(t, len(points)) for t in fields[1:]]
                if kind == "f":
                    ids.append(ids[0])
                for i in range(len(ids) - 1):
                    a, b = ids[i], ids[i + 1]
                    if a != b:
                        seen.add(a << 16 | b if a < b else b << 16 | a)
                if len(seen) > MAX_EDGES:
                    raise ValueError("too many edges")
    if not seen:
        raise ValueError("no edges")

    mid = [(min(p[i] for p in points) + max(p[i] for p in points)) / 2 for i in range(3)]
    reach = 0.0
    for p in points:
        reach = max(reach, sum((p[i] - mid[i]) ** 2 for i in range(3)))
    if reach == 0:
        raise ValueError("all vertices coincide")
    scale = RADIUS / math.sqrt(reach)

    verts = array.array("h")
    for p in points:
        for i in range(3):
            verts.append(round((p[i] - mid[i]) * scale))
    edges = array.array("H")
    for e in seen:
        edges.append(e >> 16)
        edges.append(e & 0xFFFF)
    return verts, edges


def project(verts, screen, yaw, pitch):
    """Write x,y screen positions into screen. Allocates nothing, so no GC pause during a frame."""
    sy = SIN[yaw]
    cy = COS[yaw]
    sp = SIN[pitch]
    cp = COS[pitch]
    out = 0
    for i in range(0, len(verts), 3):
        x = verts[i]
        y = verts[i + 1]
        z = verts[i + 2]
        rx = (x * cy + z * sy) >> Q
        rz = (z * cy - x * sy) >> Q
        ry = (y * cp - rz * sp) >> Q
        rz = (y * sp + rz * cp) >> Q
        d = DISTANCE - rz
        screen[out] = CX + rx * FOCAL // d
        screen[out + 1] = CY - ry * FOCAL // d
        out += 2


def draw_edges(fb, screen, edges):
    """Lines are 2 px wide: 1 px lines fade under the fast delta waveform."""
    line = fb.line
    for i in range(0, len(edges), 2):
        a = edges[i] * 2
        b = edges[i + 1] * 2
        x0 = screen[a]
        y0 = screen[a + 1]
        x1 = screen[b]
        y1 = screen[b + 1]
        line(x0, y0, x1, y1, 1)
        if abs(x1 - x0) > abs(y1 - y0):
            line(x0, y0 + 1, x1, y1 + 1, 1)
        else:
            line(x0 + 1, y0, x1 + 1, y1, 1)


class Model3D(BaseApp):
    owns_menu_buttons = True

    def __init__(self):
        super().__init__("3D Model")
        self.verts = None
        self.edges = None
        self.screen = None
        self.error = None
        self.yaw = START_YAW
        self.pitch = START_PITCH
        self.dirty = False

    def _launch(self):
        if self.verts is None:
            try:
                self.verts, self.edges = load_obj(MESH_PATH)
                self.screen = array.array("h", bytes(len(self.verts) // 3 * 4))
                self.error = None
            except (OSError, ValueError, IndexError, MemoryError) as ex:
                print("model3d: can't load", MESH_PATH, ex)
                self.error = "Can't load %s: %s" % (MESH_PATH, ex)
            gc.collect()
        # The button press that launched the app is still in the queue.
        button.clear_presses()
        display.red.fill(0)
        # The first frame clears the previous app's screen and any ghosting.
        display.request_clean()
        self.dirty = True

    def _read_presses(self):
        while True:
            p = button.next_press()
            if p is None:
                return
            yaw, pitch = PRESS_STEP[p[0]]
            self.yaw = (self.yaw + yaw) % STEPS
            self.pitch = (self.pitch + pitch) % STEPS
            self.dirty = True

    def _draw(self):
        fb = display.black
        fb.fill(0)
        if self.error:
            fb.text(self.error, 16, CY, 1)
            return
        project(self.verts, self.screen, self.yaw, self.pitch)
        draw_edges(fb, self.screen, self.edges)

    async def _show_frame(self):
        self.dirty = False
        self._draw()
        full = display.delta_frames_left() == 0
        if full:
            leds.fill(FULL_REFRESH_LEDS)
        # The app switcher has no exit hook, so fast_frames is only set while this coroutine
        # holds the app loop. Presses arriving during a refresh are applied to the next frame.
        display.fast_frames = True
        display.update()
        while not display.is_done_updating():
            self._read_presses()
            await aio.sleep_ms(20)
        display.fast_frames = False
        if full:
            leds.fill((0, 0, 0))

    async def arun(self):
        if self.launched:
            self._launch()
        self._read_presses()
        if self.dirty and display.is_done_updating():
            await self._show_frame()
        await aio.sleep_ms(20)
