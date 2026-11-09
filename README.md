# Supercon badge firmware

Modified firmware for the [Supercon 2026 Billboard eink badge](https://github.com/Hack-a-Day/2026-Billboard_Badge). Features two custom apps! 

## Custom Apps

| Ink | Quadrant |
|----|-----|
| Use my badge as a canvas! | Play a rhythm game using the buttons on the back of the screen |
| <img width="550" height="452" alt="image" src="https://github.com/user-attachments/assets/c65c210c-bb7b-41b6-b67a-fe571879547f" /> | <img width="1036" height="918" alt="image" src="https://github.com/user-attachments/assets/e8e49963-3c37-4c17-ba09-35ffccc0b089" /> | 

## 3D model viewer

The `model3d` overlay draws a mesh as a black wireframe. Each press orbits the view by 15 degrees: Menu + and Menu - change yaw, User A and User B change pitch. Hold both menu buttons to return to the app menu. The LEDs light blue during a full refresh, which takes about 6 seconds; presses made during one are applied to the next frame.

The mesh is `overlays/model3d/data/model3d/robot.obj`, a Wavefront OBJ file. Only `v`, `f` and `l` lines are read, and it can have up to 512 vertices and 1024 unique edges. It is parsed once at launch and scaled to fit the screen at every rotation. Rotation uses integer math with a sine table, so the view is an exact function of the number of presses.

`tools/model3d_check.py` tests the input handling in the simulator. `tools/model3d_bench.py` measures draw time on the badge.

## Build and flash

```sh
python3 -m venv .venv && .venv/bin/pip install littlefs-python esptool mpremote mpy-cross
.venv/bin/python tools/build.py custom                          # custom apps only
.venv/bin/python tools/build.py supercon custom                 # default Supercon apps plus yours
.venv/bin/python tools/build.py custom --flash /dev/ttyACM0     # build and write to the badge
```

| Env | Default | |
| --- | --- | --- |
| `ADDR` | `:8080` | |
| `DATABASE_URL` | required | Postgres URL: canvas, paint log, widgets, Quadrant songs and runs |
| `SITE_URL` | required | Public URL. Used for Quadrant claim links and old-domain redirects |
| `INK_LEGACY_HOST`, `QUADRANT_LEGACY_HOST` | empty | Old domains to redirect to `/ink` and `/quadrant` |
| `IMPORT_SQLITE` | empty | Path to an `ink.db` from the SQLite version (schema 3). Copied in on startup when the database has no paint or widgets yet |
| `ADMIN_PASSWORD` | empty, which disables `/admin` | Basic auth password, any username |
| `PAINT_COOLDOWN` | `30s` | Per IP and per cookie. `0s` turns it off |
| `WIFI_SSID`, `WIFI_PASSWORD` | empty | Network prefilled on `/ink/install`. Anyone can read it from the page |
| `TRUSTED_PROXY_HEADER` | empty (`X-Forwarded-For` in Docker) | Header the client IP comes from. Use `CF-Connecting-IP` behind Cloudflare |
