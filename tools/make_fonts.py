"""Render the Quadrant fonts to 1-bit glyph files the badge blits with framebuf.

    .venv/bin/python tools/make_fonts.py

Needs Pillow. The TTFs (SIL OFL) are downloaded to build/fonts/ on first run.

File format, little-endian:
    b"QF1", u16 height, u16 row of the middle of a capital H, u8 first char, u8 count,
    count x u16 width, then each glyph's rows, MONO_HLSB, (width + 7) // 8 bytes per row.
Rows above the tallest glyph and below the lowest are cropped.
"""

import struct
import urllib.request
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parent.parent
CACHE = ROOT / "build" / "fonts"
OUT = ROOT / "overlays" / "quadrant" / "data" / "quadrant"

TTF = {
    "bungee": "https://github.com/google/fonts/raw/main/ofl/bungee/Bungee-Regular.ttf",
    "chakra-bold": "https://github.com/google/fonts/raw/main/ofl/chakrapetch/ChakraPetch-Bold.ttf",
    "chakra-semibold": "https://github.com/google/fonts/raw/main/ofl/chakrapetch/ChakraPetch-SemiBold.ttf",
}
ASCII = "".join(chr(c) for c in range(32, 127))

# name: (ttf, pixel size, characters)
FONTS = {
    "title": ("bungee", 28, ASCII),
    "big": ("bungee", 44, ASCII),
    "huge": ("bungee", 60, ASCII),
    "body": ("chakra-bold", 21, ASCII),
    "small": ("chakra-semibold", 16, ASCII),
    "grade": ("bungee", 230, "ABCDS"),
}


def ttf_path(name):
    path = CACHE / (name + ".ttf")
    if not path.exists():
        CACHE.mkdir(parents=True, exist_ok=True)
        urllib.request.urlretrieve(TTF[name], path)
    return path


def render(ttf, size, chars):
    font = ImageFont.truetype(str(ttf_path(ttf)), size)
    ascent, descent = font.getmetrics()
    height = ascent + descent
    first, last = ord(min(chars)), ord(max(chars))
    images = []
    for code in range(first, last + 1):
        ch = chr(code)
        if ch not in chars:
            images.append(None)
            continue
        img = Image.new("1", (max(1, round(font.getlength(ch))), height), 0)
        draw = ImageDraw.Draw(img)
        draw.fontmode = "1"
        draw.text((0, 0), ch, font=font, fill=1)
        images.append(img)
    boxes = [img.getbbox() for img in images if img and img.getbbox()]
    top, bottom = min(b[1] for b in boxes), max(b[3] for b in boxes)
    probe = "H" if "H" in chars else chars[0]
    cap = images[ord(probe) - first].getbbox()
    mid = (cap[1] + cap[3]) // 2 - top
    widths, data = [], bytearray()
    for img in images:
        if img is None:
            widths.append(0)
            continue
        widths.append(img.width)
        # PIL's mode "1" packs rows MSB first, padded to a byte: the same as MONO_HLSB.
        data += img.crop((0, top, img.width, bottom)).tobytes()
    header = struct.pack("<3sHHBB", b"QF1", bottom - top, mid, first, len(widths))
    return header + struct.pack(f"<{len(widths)}H", *widths) + data


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    for name, (ttf, size, chars) in FONTS.items():
        blob = render(ttf, size, chars)
        (OUT / f"{name}.qf").write_bytes(blob)
        print(f"{name}.qf {len(blob) // 1024} KiB")


if __name__ == "__main__":
    main()
