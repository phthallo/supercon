"""Build the badge's littlefs partition from firmware/ plus overlays, and optionally flash it.

    .venv/bin/python tools/build.py supercon custom
    .venv/bin/python tools/build.py custom --flash /dev/ttyACM0
    .venv/bin/python tools/build.py custom --flash /dev/ttyACM0 --fresh
    .venv/bin/python tools/build.py ink --release

--release writes the files the ink site's install page serves to server/firmware/.

Overlays are directories under overlays/, applied in the order given; a later
overlay's file replaces an earlier one at the same path.
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
FIRMWARE = ROOT / "firmware"
OVERLAYS = ROOT / "overlays"
BUILD = ROOT / "build"
BACKUP = ROOT / "extracted" / "supercon_backup.bin"
RELEASE = ROOT / "server" / "firmware"

# From the partition table in supercon_backup.bin.
NVS_OFFSET, NVS_SIZE = 0x9000, 0x6000
VFS_OFFSET = 0x310000
VFS_SIZE = 0x4F0000
BLOCK_SIZE = 4096

# Written by the badge at runtime; never ship stale copies.
RUNTIME_FILES = {"data/user_apps.py", "data/app_list", "data/panel_config.json"}
# Discovered by the app switcher but not apps themselves.
NON_APPS = {"__init__.py", "base_app.py", "template.py"}


def collect(overlays):
    """Map device path -> source file, later layers winning."""
    files = {}
    for layer in [FIRMWARE] + [OVERLAYS / name for name in overlays]:
        if not layer.is_dir():
            sys.exit(f"no such overlay: {layer}")
        for src in sorted(layer.rglob("*")):
            if not src.is_file() or "__pycache__" in src.parts:
                continue
            rel = src.relative_to(layer).as_posix()
            if rel in RUNTIME_FILES:
                continue
            files[rel] = src
    return files


def set_ink_server(files):
    """Add SITE_URL to data/ink.json, unless an overlay's ink.json already names a server."""
    if "data/ink.json" not in files:
        return
    config = json.loads(files["data/ink.json"].read_text())
    if "server" in config:
        return
    site_url = os.environ.get("SITE_URL")
    if not site_url:
        sys.exit("SITE_URL is empty; set it to the site the ink app should use")
    config["server"] = site_url
    generated = BUILD / "ink.json"
    generated.write_text(json.dumps(config))
    files["data/ink.json"] = generated


def build_image(files, kept=None):
    """kept maps device path -> bytes for badge files to carry over where files has no entry."""
    # Imported here so tools/sim/badge-sim can use collect() without the venv.
    from littlefs import LittleFS

    contents = {rel: src.read_bytes() for rel, src in files.items()}
    for rel, data in (kept or {}).items():
        contents.setdefault(rel, data)
    fs = LittleFS(block_size=BLOCK_SIZE, block_count=VFS_SIZE // BLOCK_SIZE)
    for rel, data in sorted(contents.items()):
        parent = rel.rsplit("/", 1)[0] if "/" in rel else ""
        if parent:
            fs.makedirs(parent, exist_ok=True)
        with fs.open(rel, "wb") as out:
            out.write(data)
    return bytes(fs.context.buffer)


def read_badge_data(esptool, port):
    """Read the badge's filesystem from flash and return its data/ files, minus RUNTIME_FILES.

    Reading flash goes through the bootloader, so it works whatever the badge app is doing."""
    from littlefs import LittleFS, UserContext

    dump = BUILD / "badge-vfs.bin"
    subprocess.run([str(esptool), "--port", port, "read-flash", hex(VFS_OFFSET), hex(VFS_SIZE), str(dump)], check=True)
    fs = LittleFS(
        context=UserContext(buffer=bytearray(dump.read_bytes())),
        block_size=BLOCK_SIZE,
        block_count=VFS_SIZE // BLOCK_SIZE,
    )
    kept = {}
    for root, _, names in fs.walk("data"):
        for name in names:
            rel = f"{root}/{name}"
            if rel not in RUNTIME_FILES:
                with fs.open(rel, "rb") as f:
                    kept[rel] = f.read()
    return kept


def release(files, vfs):
    """Write the install page's files: the ink app on its own, and a whole-flash image.

    The web flasher erases the whole chip first, so the image carries the bootloader, partition
    table and MicroPython from the backup, with this badge's NVS blanked.
    """
    if "data/wifi.json" in files:
        sys.exit("refusing to release an image containing data/wifi.json")
    if "apps/ink.py" not in files:
        sys.exit("--release needs the ink overlay")
    RELEASE.mkdir(exist_ok=True)
    if BACKUP.exists():
        base = bytearray(BACKUP.read_bytes()[:VFS_OFFSET])
        base[NVS_OFFSET : NVS_OFFSET + NVS_SIZE] = b"\xff" * NVS_SIZE
        # Erased flash reads 0xFF, so trailing 0xFF never needs writing.
        (RELEASE / "base.bin").write_bytes(bytes(base).rstrip(b"\xff"))
    elif not (RELEASE / "base.bin").exists():
        sys.exit(f"need {BACKUP.relative_to(ROOT)} or an existing {RELEASE.relative_to(ROOT)}/base.bin")
    (RELEASE / "vfs.bin").write_bytes(vfs.rstrip(b"\xff"))
    shutil.copy2(files["apps/ink.py"], RELEASE / "ink.py")
    manifest = {
        "name": "ink badge",
        "version": time.strftime("%Y-%m-%d %H:%M"),
        "new_install_improv_wait_time": 0,
        "builds": [
            {
                "chipFamily": "ESP32-S3",
                "parts": [{"path": "base.bin", "offset": 0}, {"path": "vfs.bin", "offset": VFS_OFFSET}],
            }
        ],
    }
    (RELEASE / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    sizes = ", ".join(f"{p.name} {p.stat().st_size // 1024} KiB" for p in sorted(RELEASE.glob("*.*")))
    print(f"release -> {RELEASE.relative_to(ROOT)}: {sizes}")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("overlays", nargs="+", help="overlay names under overlays/")
    parser.add_argument(
        "--flash",
        metavar="PORT",
        help="write the image to the badge on this serial port, keeping its data/ files that the image doesn't ship",
    )
    parser.add_argument("--fresh", action="store_true", help="with --flash, erase the badge's data/ instead of keeping it")
    parser.add_argument("--stage", action="store_true", help="also copy the merged tree to build/fs/ for inspection")
    parser.add_argument("--release", action="store_true", help="write the install page's files to server/firmware/")
    args = parser.parse_args()

    files = collect(args.overlays)
    apps = [rel for rel in files if rel.startswith("apps/") and rel.count("/") == 1 and rel.endswith(".py")]
    if not [rel for rel in apps if rel.split("/")[1] not in NON_APPS]:
        sys.exit("no apps in the selected overlays; the app switcher divides by the app count and would crash")

    BUILD.mkdir(exist_ok=True)
    set_ink_server(files)
    image = build_image(files)
    out = BUILD / "vfs.bin"
    out.write_bytes(image)

    used = sum(src.stat().st_size for src in files.values())
    print(f"{len(files)} files, {used / 1024:.0f} KiB of {VFS_SIZE / 1024:.0f} KiB -> {out.relative_to(ROOT)}")

    if args.stage:
        stage = BUILD / "fs"
        shutil.rmtree(stage, ignore_errors=True)
        for rel, src in files.items():
            dest = stage / rel
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(src, dest)
        print(f"staged tree -> {stage.relative_to(ROOT)}")

    if args.release:
        release(files, image)

    if args.flash:
        esptool = Path(sys.executable).parent / "esptool"
        if not args.fresh:
            kept = read_badge_data(esptool, args.flash)
            carried = sorted(rel for rel in kept if rel not in files)
            print(f"keeping {len(carried)} files from the badge's data/: {', '.join(carried) or 'none'}")
            out = BUILD / "vfs-flash.bin"
            out.write_bytes(build_image(files, kept))
        subprocess.run(
            [str(esptool), "--port", args.flash, "write-flash", hex(VFS_OFFSET), str(out)],
            check=True,
        )


if __name__ == "__main__":
    main()
