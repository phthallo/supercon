"""Textured mesh viewer. The README lists the controls and the mesh and texture formats."""

import array
import asyncio as aio
import gc
import math

import micropython

from apps.base_app import BaseApp
from lib import button
from lib.epd import display
from lib.led_bar import leds

MESH_PATH = "/data/model3d/robot.obj"
TEXTURE_PATH = "/data/model3d/robot.pgm"
MAX_VERTS = 1024
MAX_TRIS = 2048

W, H, CX, CY = 960, 672, 480, 336

# The view is two indices into these tables, so a full turn returns to exactly the same frame.
STEPS = 24
Q = 14
SIN = [round(math.sin(2 * math.pi * i / STEPS) * (1 << Q)) for i in range(STEPS)]
COS = [SIN[(i + STEPS // 4) % STEPS] for i in range(STEPS)]

# Vertices are scaled to lie within RADIUS of the origin. A sphere of that radius seen from
# DISTANCE projects to FOCAL * RADIUS / sqrt(DISTANCE^2 - RADIUS^2) = 320 px, so every vertex
# stays on the panel. The rasterizer relies on this: it does not clip.
RADIUS = 1024
DISTANCE = 3 * RADIUS
FOCAL = 905

START_YAW = 2
START_PITCH = 1

# (yaw, pitch) change, by button id from lib.button.next_press().
PRESS_STEP = ((-1, 0), (1, 0), (0, 1), (0, -1))

FULL_REFRESH_LEDS = (0, 0, 1)

# Fractional bits of the rasterizer's edge and texture positions.
FX = 12
# Sorts after any real depth key.
NOT_DRAWN = 1 << 29
DEPTH_OFFSET = 3 * (RADIUS + 16)
INDEX_BITS = 12

# 4x4 Bayer matrix as gray thresholds. A fixed screen-space pattern keeps unchanged areas
# identical between frames, so delta frames only drive pixels that really changed.
BAYER = bytes(v * 16 + 8 for v in (0, 8, 2, 10, 12, 4, 14, 6, 3, 11, 1, 9, 15, 7, 13, 5))


def _indices(token, verts, uvs):
    fields = token.split("/")
    i = int(fields[0])
    t = int(fields[1])
    i = i - 1 if i > 0 else verts + i
    t = t - 1 if t > 0 else uvs + t
    if not 0 <= i < verts or not 0 <= t < uvs:
        raise ValueError("bad index")
    return i, t


def load_obj(path, tex_w, tex_h):
    """Read v, vt and f lines. Returns arrays: x,y,z in model units; u,v in texels; v0,v1,v2,t0,t1,t2."""
    points = []
    uvs = array.array("H")
    tris = array.array("H")
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
            elif kind == "vt":
                uvs.append(min(tex_w - 1, max(0, round(float(fields[1]) * tex_w))))
                uvs.append(min(tex_h - 1, max(0, round(float(fields[2]) * tex_h))))
            elif kind == "f":
                ids = [_indices(t, len(points), len(uvs) // 2) for t in fields[1:]]
                for i in range(1, len(ids) - 1):
                    if len(tris) // 6 == MAX_TRIS:
                        raise ValueError("too many triangles")
                    for corner in (ids[0], ids[i], ids[i + 1]):
                        tris.append(corner[0])
                    for corner in (ids[0], ids[i], ids[i + 1]):
                        tris.append(corner[1])
    if not tris:
        raise ValueError("no triangles")

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
    return verts, uvs, tris


def load_pgm(path):
    """Read an 8-bit binary PGM. Returns (width, height, pixels)."""
    with open(path, "rb") as f:
        if f.readline().strip() != b"P5":
            raise ValueError("texture is not a binary PGM")
        w, h = (int(n) for n in f.readline().split())
        if int(f.readline()) != 255:
            raise ValueError("texture is not 8-bit")
        pixels = bytearray(w * h)
        if f.readinto(pixels) != w * h:
            raise ValueError("texture is truncated")
    return w, h, pixels


def project(verts, screen, depth, yaw, pitch):
    """Write x,y screen positions into screen and camera-facing depth into depth."""
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
        depth[i // 3] = rz
        out += 2


def visible_triangles(screen, depth, tris, order):
    """Fill order with keys of front-facing triangles, farthest first. Returns how many."""
    n = 0
    t = 0
    for index in range(len(tris) // 6):
        a = tris[t]
        b = tris[t + 1]
        c = tris[t + 2]
        ax = screen[a * 2]
        ay = screen[a * 2 + 1]
        area = (screen[b * 2] - ax) * (screen[c * 2 + 1] - ay) - (screen[c * 2] - ax) * (screen[b * 2 + 1] - ay)
        if area < 0:
            order[n] = (depth[a] + depth[b] + depth[c] + DEPTH_OFFSET) << INDEX_BITS | index
            n += 1
        t += 6
    for i in range(n, len(order)):
        order[i] = NOT_DRAWN
    order.sort()
    return n


@micropython.viper
def _fill_spans(fb, tex, params, bayer):
    """Fill the rows of one flat-edged part of a triangle. Unpacks params, see Rasterizer."""
    pixels = ptr8(fb)
    texels = ptr8(tex)
    p = ptr32(params)
    thresholds = ptr8(bayer)
    y = int(p[0])
    y_end = int(p[1])
    xl = int(p[2])
    dxl = int(p[3])
    xr = int(p[4])
    dxr = int(p[5])
    ul = int(p[6])
    dul = int(p[7])
    vl = int(p[8])
    dvl = int(p[9])
    dux = int(p[10])
    dvx = int(p[11])
    tex_w = int(p[12])
    tex_size = uint(p[13])
    # ptr32 loads zero-extend on the 64-bit simulator. wrap is 2^32 there and 0 on the badge.
    wrap = 0x10000
    wrap = wrap * wrap
    dxl -= ((dxl >> 31) & 1) * wrap
    dxr -= ((dxr >> 31) & 1) * wrap
    ul -= ((ul >> 31) & 1) * wrap
    dul -= ((dul >> 31) & 1) * wrap
    vl -= ((vl >> 31) & 1) * wrap
    dvl -= ((dvl >> 31) & 1) * wrap
    dux -= ((dux >> 31) & 1) * wrap
    dvx -= ((dvx >> 31) & 1) * wrap
    while y < y_end:
        x = (xl + 4095) >> 12
        x_end = (xr + 4095) >> 12
        u = ul
        v = vl
        row = y * 120
        band = (y & 3) << 2
        while x < x_end:
            t = (v >> 12) * tex_w + (u >> 12)
            if uint(t) < tex_size:
                at = row + (x >> 3)
                mask = 0x80 >> (x & 7)
                if texels[t] < thresholds[band | (x & 3)]:
                    pixels[at] = pixels[at] | mask
                else:
                    pixels[at] = pixels[at] & (mask ^ 0xFF)
            u += dux
            v += dvx
            x += 1
        xl += dxl
        xr += dxr
        ul += dul
        vl += dvl
        y += 1


class Rasterizer:
    def __init__(self, fb, tex, tex_w, tex_h):
        self.fb = fb
        self.tex = tex
        self.p = array.array("i", [0] * 14)
        self.p[12] = tex_w
        self.p[13] = tex_w * tex_h

    def triangle(self, x0, y0, u0, v0, x1, y1, u1, v1, x2, y2, u2, v2):
        """Fill a triangle whose corners are sorted by y."""
        height = y2 - y0
        dy1 = y1 - y0
        denom = (x1 - x0) * height - (x2 - x0) * dy1
        if denom == 0:
            return
        p = self.p
        p[10] = (((u1 - u0) * height - (u2 - u0) * dy1) << FX) // denom
        p[11] = (((v1 - v0) * height - (v2 - v0) * dy1) << FX) // denom
        fx0 = x0 << FX
        fu0 = u0 << FX
        fv0 = v0 << FX
        long_dx = ((x2 - x0) << FX) // height
        long_du = ((u2 - u0) << FX) // height
        long_dv = ((v2 - v0) << FX) // height
        # denom > 0 means vertex 1 is right of the long edge.
        long_is_left = denom > 0
        if dy1 > 0:
            dx = ((x1 - x0) << FX) // dy1
            if long_is_left:
                self._spans(y0, y1, fx0, long_dx, fu0, long_du, fv0, long_dv, fx0, dx)
            else:
                du = ((u1 - u0) << FX) // dy1
                dv = ((v1 - v0) << FX) // dy1
                self._spans(y0, y1, fx0, dx, fu0, du, fv0, dv, fx0, long_dx)
        if y2 > y1:
            lower = y2 - y1
            dx = ((x2 - x1) << FX) // lower
            long_x = fx0 + long_dx * dy1
            if long_is_left:
                self._spans(y1, y2, long_x, long_dx, fu0 + long_du * dy1, long_du, fv0 + long_dv * dy1, long_dv, x1 << FX, dx)
            else:
                du = ((u2 - u1) << FX) // lower
                dv = ((v2 - v1) << FX) // lower
                self._spans(y1, y2, x1 << FX, dx, u1 << FX, du, v1 << FX, dv, long_x, long_dx)

    def _spans(self, y, y_end, xl, dxl, ul, dul, vl, dvl, xr, dxr):
        p = self.p
        p[0] = y
        p[1] = y_end
        p[2] = xl
        p[3] = dxl
        p[4] = xr
        p[5] = dxr
        p[6] = ul
        p[7] = dul
        p[8] = vl
        p[9] = dvl
        _fill_spans(self.fb, self.tex, p, BAYER)


def draw_triangles(raster, screen, uvs, tris, order, count):
    for i in range(count):
        t = (order[i] & ((1 << INDEX_BITS) - 1)) * 6
        a = tris[t] * 2
        b = tris[t + 1] * 2
        c = tris[t + 2] * 2
        ta = tris[t + 3] * 2
        tb = tris[t + 4] * 2
        tc = tris[t + 5] * 2
        # Swaps stay at two values: larger ones allocate a tuple, and the resulting garbage
        # collections stall a frame for 2-3 seconds.
        if screen[b + 1] < screen[a + 1]:
            a, b = b, a
            ta, tb = tb, ta
        if screen[c + 1] < screen[a + 1]:
            a, c = c, a
            ta, tc = tc, ta
        if screen[c + 1] < screen[b + 1]:
            b, c = c, b
            tb, tc = tc, tb
        raster.triangle(
            screen[a], screen[a + 1], uvs[ta], uvs[ta + 1],
            screen[b], screen[b + 1], uvs[tb], uvs[tb + 1],
            screen[c], screen[c + 1], uvs[tc], uvs[tc + 1],
        )


class Model3D(BaseApp):
    owns_menu_buttons = True

    def __init__(self):
        super().__init__("3D Model")
        self.verts = None
        self.error = None
        self.yaw = START_YAW
        self.pitch = START_PITCH
        self.dirty = False

    def _load(self):
        try:
            tex_w, tex_h, tex = load_pgm(TEXTURE_PATH)
            self.verts, self.uvs, self.tris = load_obj(MESH_PATH, tex_w, tex_h)
            count = len(self.verts) // 3
            self.screen = array.array("h", bytes(count * 4))
            self.depth = array.array("h", bytes(count * 2))
            self.order = [NOT_DRAWN] * (len(self.tris) // 6)
            self.raster = Rasterizer(display.black, tex, tex_w, tex_h)
            self.error = None
        except (OSError, ValueError, IndexError, MemoryError) as ex:
            print("model3d: can't load mesh", ex)
            self.verts = None
            self.error = "Can't load model: %s" % ex
        gc.collect()

    def _launch(self):
        if self.verts is None:
            self._load()
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
        project(self.verts, self.screen, self.depth, self.yaw, self.pitch)
        count = visible_triangles(self.screen, self.depth, self.tris, self.order)
        draw_triangles(self.raster, self.screen, self.uvs, self.tris, self.order, count)

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
