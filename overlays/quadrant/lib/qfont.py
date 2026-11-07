"""1-bit fonts from tools/make_fonts.py, drawn onto a framebuf with blit."""

import framebuf
import struct

# blit palette that swaps 0 and 1; with key 1 it draws a glyph's set pixels as white.
_INVERT = framebuf.FrameBuffer(bytearray(1), 2, 1, framebuf.MONO_HLSB)
_INVERT.pixel(0, 0, 1)

LEFT = 0
CENTER = 1
RIGHT = 2


class Font:
    def __init__(self, path):
        with open(path, "rb") as f:
            data = bytearray(f.read())
        _, self.height, self.mid, self.first, count = struct.unpack_from("<3sHHBB", data)
        self.widths = struct.unpack_from("<%dH" % count, data, 9)
        self.glyphs = []
        mv = memoryview(data)
        pos = 9 + 2 * count
        for w in self.widths:
            if not w:
                self.glyphs.append(None)
                continue
            n = (w + 7) // 8 * self.height
            self.glyphs.append(framebuf.FrameBuffer(mv[pos : pos + n], w, self.height, framebuf.MONO_HLSB))
            pos += n

    def width(self, s, track=0):
        total = 0
        first, widths = self.first, self.widths
        for ch in s:
            i = ord(ch) - first
            if 0 <= i < len(widths) and widths[i]:
                total += widths[i] + track
        return total - track if total else 0

    def draw(self, fb, s, x, y, color=1, align=LEFT, track=0):
        """Draw s with the middle of its capitals on row y. Returns the x after the text."""
        if align:
            x -= self.width(s, track) // (2 if align == CENTER else 1)
        y -= self.mid
        first, widths, glyphs = self.first, self.widths, self.glyphs
        for ch in s:
            i = ord(ch) - first
            if not (0 <= i < len(widths)) or not widths[i]:
                continue
            if color:
                fb.blit(glyphs[i], x, y, 0)
            else:
                fb.blit(glyphs[i], x, y, 1, _INVERT)
            x += widths[i] + track
        return x
