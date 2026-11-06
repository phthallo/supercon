# Supercon badge firmware

MicroPython filesystem for the Supercon e-paper badge (ESP32-S3), split out of `supercon_backup.bin`.

| Path | Contents |
| --- | --- |
| `firmware/` | Generic runtime: boot, app switcher, display/button/LED/SAO drivers, `BaseApp`, app template |
| `overlays/supercon/` | Event content: schedule, gallery, nametag apps and their images, `data/splash.png` |
| `overlays/custom/` | Your apps and data. Starts with `apps/hello.py` |
| `overlays/ink/` | Badge app for the shared canvas at ink.underwhelm.ing |
| `server/` | Go server for the shared canvas and widget editor |
| `tools/build.py` | Merges `firmware/` with overlays into a littlefs image and flashes it |
| `extracted/` | Untouched dump of the original filesystem, plus `supercon_backup.bin` (full 8 MiB flash, MicroPython included). Not committed |

## Build and flash

```sh
.venv/bin/python tools/build.py custom                          # your apps only
.venv/bin/python tools/build.py supercon custom                 # Supercon apps plus yours
.venv/bin/python tools/build.py custom --flash /dev/ttyACM0     # build and write to the badge
```

Overlays apply in order, and a later overlay's file replaces an earlier one at the same path. `--flash` rewrites only the `vfs` partition (0x310000). The MicroPython firmware already on the badge stays as it is. To restore the original badge:

```sh
.venv/bin/esptool --port /dev/ttyACM0 write-flash 0x0 extracted/supercon_backup.bin
```

## Writing an app

Put a module in `overlays/<name>/apps/`. Every `BaseApp` subclass in it shows up in the menu. `self.launched` is true on the first `run()` after the app is selected. Set `_abstract = True` on a shared base class to keep it out of the menu. See `firmware/apps/template.py` for buttons.

`data/splash.png` in any overlay is shown on the first boot after flashing.

## Ink canvas

A website where anyone paints a 120×84 grid in white, black and red, one pixel per cooldown. The admin places text, box and image widgets on top at `/admin`. The badge's Ink app downloads the combined 960×672 frame.

Badge:

```sh
cp overlays/ink/data/wifi.example.json overlays/custom/data/wifi.json   # then fill in ssid and password
.venv/bin/python tools/build.py ink custom --flash /dev/ttyACM0
```

`overlays/ink/data/ink.json` sets the server URL and how often to poll (`poll_s`). User A forces a refresh. The LEDs turn cyan while fetching, green when done and red on an error.

Server:

```sh
cd server
ADMIN_PASSWORD=secret go run .      # http://localhost:8080, admin at /admin
go test ./...
```

| Env | Default | |
| --- | --- | --- |
| `ADDR` | `:8080` | |
| `DB_PATH` | `ink.db` (`/data/ink.db` in Docker) | SQLite file: canvas, paint log, widgets |
| `ADMIN_PASSWORD` | empty, which disables `/admin` | Basic auth password, any username |
| `PAINT_COOLDOWN` | `30s` | Per IP and per cookie |
| `TRUSTED_PROXY_HEADER` | empty (`X-Forwarded-For` in Docker) | Header the client IP comes from. Use `CF-Connecting-IP` behind Cloudflare |

`GET /badge/frame` returns the black plane then the red plane (80,640 bytes each, MSB = leftmost pixel), zlib-compressed, with an ETag. `GET /badge/frame.png` returns the same frame as a PNG.
