"""Time the model3d app's transform and draw on the badge, without touching the panel.

    .venv/bin/python tools/build.py model3d --stage
    .venv/bin/mpremote connect /dev/ttyACM0 soft-reset mount build/fs run tools/model3d_bench.py
    .venv/bin/mpremote connect /dev/ttyACM0 reset
"""

import sys
import time


class Stub:
    """lvgl.init() hard-crashes after a soft reset, and nothing here draws through LVGL."""

    def __getattr__(self, _name):
        return self

    def __call__(self, *_args, **_kwargs):
        return self


sys.modules["lvgl"] = Stub()

from apps import model3d  # noqa: E402

BUDGET_MS = 400

model3d.MESH_PATH = "data/model3d/robot.obj"
model3d.TEXTURE_PATH = "data/model3d/robot.pgm"
app = model3d.Model3D()
start = time.ticks_ms()
app._launch()
if app.error:
    raise SystemExit(app.error)
print("load:", time.ticks_diff(time.ticks_ms(), start), "ms")
print("mesh:", len(app.verts) // 3, "vertices,", len(app.tris) // 6, "triangles")

times = []
for pitch in range(0, model3d.STEPS, 3):
    for yaw in range(model3d.STEPS):
        app.yaw = yaw
        app.pitch = pitch
        start = time.ticks_us()
        app._draw()
        times.append(time.ticks_diff(time.ticks_us(), start) / 1000)

times.sort()
median = times[len(times) // 2]
print("frames: %d  min %.1f ms  median %.1f ms  max %.1f ms" % (len(times), times[0], median, times[-1]))
print("PASS" if median < BUDGET_MS else "FAIL", "budget", BUDGET_MS, "ms")
