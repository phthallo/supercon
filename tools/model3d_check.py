#!/usr/bin/env python3
"""Check the model3d app's input handling in the badge simulator. Needs tools/sim/build.

    python3 tools/model3d_check.py [PNG_OUTPUT]

Each run launches the app with a scripted press, sends more presses, and compares the final
e-paper image. PNG_OUTPUT, if given, receives the start view (needs ImageMagick).
"""

import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

SIM = Path(__file__).resolve().parent / "sim"

LAUNCH_MS = 3000
# After this the launch frame's full refresh (about 6 s) has finished.
SETTLED_MS = 12000
SPACING_MS = 1200
# Inside the launch frame's full refresh.
BURST_START_MS = 4500
BURST_SPACING_MS = 100
# Nets out to yaw -2, pitch +1.
SEQUENCE = ["plus", "plus", "a", "minus", "plus", "a", "b"]


def tap(ms, button):
    return [f"{ms} {button} down", f"{ms + 50} {button} up"]


def spaced(buttons, start, spacing):
    return [line for i, b in enumerate(buttons) for line in tap(start + i * spacing, b)]


def run(name, lines, exit_after):
    """Returns (epd.pbm bytes, simulator stdout)."""
    with tempfile.TemporaryDirectory() as tmp:
        script = Path(tmp) / "presses.txt"
        script.write_text("\n".join(tap(LAUNCH_MS, "a") + lines) + "\n")
        dump = Path(tmp) / "dump"
        dump.mkdir()
        # The interpreter segfaults on exit (see tools/sim/README.md), so the exit code means nothing.
        result = subprocess.run(
            [
                str(SIM / "badge-sim"), "model3d", "--real-refresh", "--sao-port", "0",
                "--input", str(script), "--dump", str(dump), "--exit-after", str(exit_after),
            ],
            env={**os.environ, "SDL_VIDEODRIVER": "offscreen"},
            capture_output=True,
            text=True,
            timeout=exit_after + 30,
        )
        image = dump / "epd.pbm"
        if not image.exists():
            sys.exit(f"{name}: no image written\n{result.stdout}\n{result.stderr}")
        print(f"ran {name}")
        return image.read_bytes(), result.stdout


def seconds_for(presses, start, spacing):
    return (start + presses * spacing) // 1000 + 4


def main():
    failures = []

    def check(ok, label):
        print(("pass: " if ok else "FAIL: ") + label)
        if not ok:
            failures.append(label)

    if not (SIM / "build" / "lvgl_micropy_unix").exists():
        sys.exit("tools/sim/build/lvgl_micropy_unix is missing; see tools/sim/README.md")
    # The simulator only copies data files that are missing, so an edited mesh would be ignored.
    shutil.rmtree(SIM / "data" / "model3d", ignore_errors=True)

    start, _ = run("start view", [], 14)
    if len(sys.argv) > 1:
        with tempfile.NamedTemporaryFile(suffix=".pbm") as pbm:
            pbm.write(start)
            pbm.flush()
            subprocess.run(["magick", pbm.name, sys.argv[1]], check=True)

    views = [start]
    for button in ("a", "b", "plus", "minus"):
        image, _ = run(f"one press of {button}", tap(SETTLED_MS, button), 16)
        check(image != start, f"{button} changes the start view")
        views.append(image)
    check(len(set(views)) == 5, "the four directions and the start view are all different")

    turn = ["minus"] * 24
    image, _ = run("full turn", spaced(turn, SETTLED_MS, SPACING_MS), seconds_for(24, SETTLED_MS, SPACING_MS))
    check(image == start, "a full yaw turn matches the start view exactly")

    slow, _ = run(
        "sequence, slow", spaced(SEQUENCE, SETTLED_MS, SPACING_MS), seconds_for(len(SEQUENCE), SETTLED_MS, SPACING_MS)
    )
    burst, out = run("sequence, burst", spaced(SEQUENCE, BURST_START_MS, BURST_SPACING_MS), 20)
    check(burst == slow, "a burst during a full refresh ends on the same view as slow presses")
    check(slow != start, "the test sequence moves the view")
    last_press = out.rfind("B up")
    check(0 <= last_press < out.find("epd: full sent"), "the burst finished before the full refresh did")

    chord = [
        f"{SETTLED_MS} plus down",
        f"{SETTLED_MS + 50} minus down",
        f"{SETTLED_MS + 900} plus up",
        f"{SETTLED_MS + 900} minus up",
    ]
    image, out = run("both menu buttons held", chord, 16)
    check("Activated app switching" in out, "holding both menu buttons opens the app menu")
    check(image == start, "the chord's two yaw presses cancel out")

    if failures:
        sys.exit(f"{len(failures)} checks failed")


if __name__ == "__main__":
    main()
