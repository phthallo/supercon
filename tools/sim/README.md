# Badge simulator

Runs `firmware/` plus overlays on a desktop, from [Hack-a-Day/2026-Billboard_Badge#11](https://github.com/Hack-a-Day/2026-Billboard_Badge/pull/11). The firmware runs unmodified; `sim.py` replaces the hardware modules.

```bash
tools/sim/badge-sim supercon custom
tools/sim/badge-sim quadrant --real-refresh --scale 0.7
```

Overlay names (or paths) come first, as in `tools/build.py`. The merged tree is copied to `tools/sim/fs/` on every run. Runtime writes go to `tools/sim/data/`; delete it to reset. Options are listed at the top of `sim.py`.

Keys: A = User A, Left = Menu +, Right = Menu -, B = User B, Space = Boot.

Use `--real-refresh` for anything timing-related. Full refreshes then take ~6.5 s and delta frames 0.17 s, as measured on driver B.

## Headless runs

```bash
SDL_VIDEODRIVER=offscreen tools/sim/badge-sim quadrant --real-refresh \
  --input presses.txt --dump out/ --exit-after 30
magick out/epd.pbm out/epd.png
```

`presses.txt` has one press edge per line: `3000 a down`, `3100 a up`. Buttons are a, b, plus, minus and boot.

## Build

Needs gcc, cmake, ninja and libffi-devel. The build compiles SDL2 itself and takes about 10 minutes.

```bash
git clone https://github.com/lvgl-micropython/lvgl_micropython tools/sim/lvgl_micropython
cd tools/sim/lvgl_micropython
sed -i -E 's/^#define LV_FONT_MONTSERRAT_([0-9]+) +0/#define LV_FONT_MONTSERRAT_\1 1/' lib/lv_conf.h
python3 make.py unix DISPLAY=sdl_display INDEV=sdl_pointer FROZEN_MANIFEST=../frozen_manifest.py
mkdir -p ../build && cp build/lvgl_micropy_unix ../build/ && chmod +x ../build/lvgl_micropy_unix
```

The interpreter segfaults on exit. That's a bug in lvgl_micropython's shutdown and is harmless.

## Differences from the badge

- The interpreter is MicroPython 1.27 with lvgl_micropython bindings, not the badge's lv_micropython 1.24 build.
- The sim reports `sys.platform` as `esp32` to firmware code, so `lib/led_bar.py` uses the fake NeoPixel.
- `os.ilistdir` entries are padded to 4 values for `lib/app_switcher.py`.
- Pin IRQs are called when a key or mouse press changes a button's level. Keyboard events are read every 33 ms, so live key timestamps jitter by up to 33 ms. Scripted `--input` presses don't.
- CPU speed, RAM limits, ghosting and the panel's flashing aren't simulated.
