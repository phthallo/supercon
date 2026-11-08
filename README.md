# Supercon badge firmware

Modified firmware for the [Supercon 2026 Billboard eink badge](https://github.com/Hack-a-Day/2026-Billboard_Badge). Features two custom apps! 

## Custom Apps

| Ink | Quadrant |
|----|-----|
| Use my badge as a canvas! | Play a rhythm game using the buttons on the back of the screen |
| | | 

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
