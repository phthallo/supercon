"""E-Paper Display Driver for Billboard Badge

Ported from Wiring in https://github.com/PervasiveDisplays/EPD_Driver_GU_large/blob/e408154652802a82e520cc25ba04adeb347d0528/src/EPD_Driver.cpp

--COPYRIGHT--
Brief The drivers and functions of development board
Copyright (c) 2012-2021 Pervasive Displays Inc. All rights reserved.
 Authors: Pervasive Displays Inc.
 Redistribution and use in source and binary forms, with or without
 modification, are permitted provided that the following conditions
 are met:
 1. Redistributions of source code must retain the above copyright
    notice, this list of conditions and the following disclaimer.
 2. Redistributions in binary form must reproduce the above copyright
    notice, this list of conditions and the following disclaimer in
    the documentation and/or other materials provided with the
    distribution.
 THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
 "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
 LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
 A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
 OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
 SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
 LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
 DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
 THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
 OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
"""

import asyncio as aio
import btree
import framebuf
import gc
import micropython
from machine import Pin, SPI
from micropython import const
import time

import lvgl

from lib import luts
from lib.large_png import show_png

EPD_CS1_PIN = Pin(13, mode=Pin.OUT)
EPD_CS2_PIN = Pin(3, mode=Pin.OUT)
EPD_BUSY1_PIN = Pin(12, mode=Pin.IN)
EPD_BUSY2_PIN = Pin(8, mode=Pin.IN)
EPD_DC_PIN = Pin(10, mode=Pin.OUT)  # Data High Command Low
EPD_RST_PIN = Pin(11, mode=Pin.OUT)
EPD_SCL_PIN = Pin(21, mode=Pin.OUT)
EPD_SDA_PIN = Pin(47, mode=Pin.OUT)
EPD_SPI_MISO_PIN = Pin(45, mode=Pin.IN)  # Not connected, signal not needed
EPD_PWR_EN_PIN = Pin(48, mode=Pin.OUT, value=False)


pdi_cp = 0x9608
pdi_size = 0x96  # 9.69"
display_width = const(960)
half_width = const(display_width // 2)
display_height = const(672)
# Frame-buffer size = width * height / 8 * depth, uint32_t
# image_data_size is really only half the panel pixels, of one color
half_frame_one_color_size = const(display_height * display_width // (8 * 2))
# The width of rows in the full-frame and half-frame framebuffers
# Counting bytes, not pixels, since each byte is 8 packed pixels
display_width_bytes = const(display_width // 8)
display_half_width_bytes = const(display_width_bytes // 2)

# Delta frames keep the DC/DC booster running between frames; these bound how long it stays up.
_SESSION_IDLE_MS = const(3000)
_SESSION_MAX_FRAMES = const(20)
# The delta waveform isn't DC-balanced per pixel, so after this many delta frames one frame
# goes through the full waveform instead.
_CLEAN_EVERY = const(300)
# A delta refresh takes ~580 ms. A refresh still running after this is hung.
_DELTA_TIMEOUT_MS = const(4000)


# Two displays use different bit orders
def reverse_byte(byte: int) -> int:
    byte = (byte & 0xF0) >> 4 | (byte & 0x0F) << 4
    byte = (byte & 0xCC) >> 2 | (byte & 0x33) << 2
    byte = (byte & 0xAA) >> 1 | (byte & 0x55) << 1
    return byte


_REVERSED = bytes(reverse_byte(i) for i in range(256))
_SAME = bytes(range(256))


# Viper functions are kept to four arguments, the limit on some MicroPython versions.
# Large transfers go out in pieces this size. Pin IRQ callbacks are scheduled, not hard, so they
# only run between Python calls: one 40 KB write would hold a button timestamp back 20 ms.
_CHUNK = const(2400)
_SPLIT_ROWS = const(56)
_split_table = bytearray(256)
_NO_ROWS = const(-1)


@micropython.viper
def _split_band(src: ptr8, left: ptr8, right: ptr8, rows: int):
    """Split rows of a full-width plane into the two COG halves, mapping each byte through _split_table."""
    table = ptr8(_split_table)
    i = 0
    o = 0
    for _ in range(rows):
        for c in range(display_half_width_bytes):
            left[o + c] = table[src[i + c]]
            right[o + c] = table[src[i + display_half_width_bytes + c]]
        i += display_width_bytes
        o += display_half_width_bytes


def _split_bands(src, left, right):
    """Slices for _split_band, made once: each memoryview slice is a heap allocation."""
    src, left, right = memoryview(src), memoryview(left), memoryview(right)
    bands = []
    for row in range(0, display_height, _SPLIT_ROWS):
        i = row * display_width_bytes
        o = row * display_half_width_bytes
        bands.append((src[i:], left[o:], right[o:], min(_SPLIT_ROWS, display_height - row)))
    return bands


def _chunks(buf):
    mv = memoryview(buf)
    return [mv[i : i + _CHUNK] for i in range(0, len(mv), _CHUNK)]


@micropython.viper
def _any_set(buf: ptr8, n: int) -> int:
    acc = 0
    for i in range(n):
        acc |= buf[i]
    return acc


@micropython.viper
def _rgb565_row(src: ptr8, black: ptr8, red: ptr8, pos_width: int):
    """Write one row of RGB565 pixels into the planes. pos_width = first pixel index << 10 | width."""
    base = pos_width >> 10
    for col in range(pos_width & 0x3FF):
        lo = src[col * 2]
        hi = src[col * 2 + 1]
        idx = base + col
        byte = idx >> 3
        mask = 0x80 >> (idx & 7)
        keep = mask ^ 0xFF
        # Top bit of each channel: red is bit 15, green bit 10, blue bit 4.
        if (hi & 0x04) or (lo & 0x10):
            black[byte] = black[byte] & keep
            red[byte] = red[byte] & keep
        elif hi & 0x80:
            red[byte] = red[byte] | mask
            black[byte] = black[byte] & keep
        else:
            black[byte] = black[byte] | mask
            red[byte] = red[byte] & keep


@micropython.viper
def _changed_rows(a: ptr8, b: ptr8) -> int:
    """Rows where two half planes differ, as first << 10 | last. -1 if none."""
    first = -1
    last = -1
    i = 0
    for row in range(display_height):
        for c in range(display_half_width_bytes):
            if a[i + c] != b[i + c]:
                if first < 0:
                    first = row
                last = row
                break
        i += display_half_width_bytes
    if first < 0:
        return -1
    return first << 10 | last


def _union_rows(a: int, b: int) -> int:
    if a < 0:
        return b
    if b < 0:
        return a
    return min(a >> 10, b >> 10) << 10 | max(a & 0x3FF, b & 0x3FF)


def _busy() -> bool:
    return not EPD_BUSY1_PIN.value() or not EPD_BUSY2_PIN.value()


def _wire(data):
    return data if isinstance(data, (bytes, bytearray, memoryview)) else bytes(data)


class EPaperDisplay:
    width = display_width
    height = display_height

    def __init__(self) -> None:
        print("Initializing E-Paper Display")
        EPD_DC_PIN.value(True)
        EPD_RST_PIN.value(True)
        EPD_CS1_PIN.value(True)
        EPD_CS2_PIN.value(True)
        # miso must be named: left out, SPI2 claims its default MISO, GPIO13, which is CS1 on this board.
        # The sheet's plane rate is 4 MHz; 16 MHz was measured clean on the badge (85 ms for both planes).
        spi_args = dict(baudrate=16_000_000, sck=EPD_SCL_PIN, mosi=EPD_SDA_PIN, miso=EPD_SPI_MISO_PIN)
        self.spi = SPI(2, **spi_args)
        # SPI2 state survives a soft reset, and re-creating it with the same settings skips
        # routing the pins, which Pin() above has just reset. deinit forces a full setup.
        self.spi.deinit()
        self.spi.init(**spi_args)

        # E2969JS084 is "driver 8"; anything else is treated as E2969JS0B1, "driver B".
        display_pn = b"B1"
        try:
            with open("/data/hwconf", "rb") as f:
                display_pn = btree.open(f).get(b"display_pn", b"B1")
        except OSError:
            pass
        self._driver_b = display_pn != b"84"
        self._reverse_fb_bits = self._driver_b
        # Unknown at boot, so assume red is showing and must be cleared.
        self._screen_red = True
        # The black plane on the film, as last sent; valid only while _film_ok.
        self._film = (bytearray(half_frame_one_color_size), bytearray(half_frame_one_color_size))
        self._film_ok = False
        # id(plane) -> its _CHUNK-sized slices, for the planes run() sends.
        self._plane_chunks = {}
        self._delta_frames = 0
        # A delta session is open: COG initialised and booster running, so a frame is planes + 0x15.
        self._hot = False
        self._session_frames = 0
        # Closing and reopening a session delays that frame by ~600 ms. Apps that need a steady frame rate raise this.
        self.session_max_frames = _SESSION_MAX_FRAMES
        # Delta frames use LUTB_FAST: ~360 ms a frame instead of ~650 ms, with lighter ink.
        self.fast_frames = False
        self._hot_fast = False
        # Per CoG, the rows the last pull changed: 0x10 still holds the frame before them.
        self._prev_rows = (_NO_ROWS, _NO_ROWS)
        self._last_frame = 0
        # A delta refresh is still running on the panel.
        self._inflight = False

        # The framebuffers are set and display should be updated.
        self._framebuffer_ready = aio.Event()

        """Each byte is a mini-row of 8 bits for pixels of one color (1 sets the color, 0 is white)
        Bytes are in rows across the screen, as seen on the badge
        0 ------------------------> 59 [60 bytes per row]
        60 ----------------------> 119
        ...                        ...
        [672 rows of 60 bytes per row]
        """
        self._framebuffer_black = bytearray(half_frame_one_color_size * 2)
        self._framebuffer_red = bytearray(half_frame_one_color_size * 2)
        # Framebuffers that can be drawn to directly
        self.black = framebuf.FrameBuffer(
            self._framebuffer_black, display_width, display_height, framebuf.MONO_HLSB
        )
        self.red = framebuf.FrameBuffer(
            self._framebuffer_red, display_width, display_height, framebuf.MONO_HLSB
        )

        # Is the display done updating? Start set since the display is not updating before boot.
        self._display_updated = aio.Event()
        self._display_updated.set()
        gc.collect()

        # Set up lvgl
        lvgl.init()
        # Give LVGL a buffer 2-bytes-per-pixel for RGB565 format
        self._lv_draw_buf = bytearray(display_width * display_height * 2)

        self.lv_display = lvgl.display_create(display_width, display_height)
        self.lv_display.set_color_format(lvgl.COLOR_FORMAT.RGB565)
        self.lv_display.set_buffers(
            self._lv_draw_buf, None, len(self._lv_draw_buf),
            lvgl.DISPLAY_RENDER_MODE.PARTIAL,
        )
        self.lv_display.set_flush_cb(self._lv_flush)
        self.lv_screen = lvgl.screen_active()
        self.lv_screen.set_size(display_width, display_height)

    def _lv_flush(self, disp, area, color_p):
        x1, y1 = area.x1, area.y1
        w = area.x2 - x1 + 1
        h = area.y2 - y1 + 1
        try:
            stride = lvgl.draw_buf_width_to_stride(w, lvgl.COLOR_FORMAT.RGB565)
        except AttributeError:
            stride = w * 2
        src = memoryview(color_p.__dereference__(stride * h))
        black, red = self._framebuffer_black, self._framebuffer_red
        for row in range(h):
            pos = (y1 + row) * display_width + x1
            _rgb565_row(src[row * stride :], black, red, pos << 10 | w)
        disp.flush_ready()

    def update(self):
        """Done updating the framebuffer, update the display with the contents."""
        self._framebuffer_ready.set()

    def clear(self):
        """Clear framebuffers to full white"""
        self.black.fill(0)
        self.red.fill(0)

    def is_done_updating(self):
        """Is the display done updating? False while an update() is still queued."""
        return self._display_updated.is_set() and not self._framebuffer_ready.is_set()

    def draw_image(self, image, height: int, width: int, row: int = 0, col: int = 0):
        for img_r in range(height):
            for img_c in range(width):
                color = image[img_r][img_c]
                red = color[0] > 127
                black = color[0] < 128 and color[1] < 128 and color[2] < 128
                self.black.pixel((col + img_c), (row + img_r), int(black))
                self.red.pixel((col + img_c), (row + img_r), int(red))

    def draw_png(self, filename: str, row: int = 0, col: int = 0):

        gc.collect()
        print(f"Loading {filename} This will take a while... {time.time()}")
        show_png(filename, self._framebuffer_black, self._framebuffer_red, row, col)
        print("Done loading png")
        gc.collect()

    async def run(self):
        # black_left, red_left, black_right, red_right
        halves = tuple(bytearray(half_frame_one_color_size) for _ in range(4))
        _split_table[:] = _REVERSED if self._reverse_fb_bits else _SAME
        black_left, red_left, black_right, red_right = halves
        black_bands = _split_bands(self._framebuffer_black, black_left, black_right)
        red_bands = _split_bands(self._framebuffer_red, red_left, red_right)
        for h in halves + self._film:
            self._plane_chunks[id(h)] = _chunks(h)
        # The red halves still hold the last frame's red and need one more split to clear.
        red_stale = False
        while True:
            if self._framebuffer_ready.is_set():
                self._display_updated.clear()
                # Cleared before the copy, so an update() during the refresh queues another one.
                self._framebuffer_ready.clear()
                has_red = bool(_any_set(self._framebuffer_red, half_frame_one_color_size * 2))
                for band in black_bands:
                    _split_band(*band)
                if has_red or red_stale:
                    for band in red_bands:
                        _split_band(*band)
                red_stale = has_red
                delta = await self.global_update(black_left, red_left, black_right, red_right, has_red)
                # A collection takes ~140 ms with nothing else running, so frame-rate apps skip it.
                if not delta:
                    gc.collect()
                self._display_updated.set()
            elif self._hot and time.ticks_diff(time.ticks_ms(), self._last_frame) > _SESSION_IDLE_MS:
                await self._close_session()
            await aio.sleep_ms(20)

    async def global_update(
        self,
        black_left: bytearray | bytes | memoryview[int],
        red_left: bytearray | bytes | memoryview[int],
        black_right: bytearray | bytes | memoryview[int],
        red_right: bytearray | bytes | memoryview[int],
        has_red: bool,
    ):
        left_rows = right_rows = _NO_ROWS
        if self._can_delta(has_red):
            left_rows = _changed_rows(black_left, self._film[0])
            right_rows = _changed_rows(black_right, self._film[1])
            if left_rows < 0 and right_rows < 0:
                return True
        waited = time.ticks_ms()
        await self._finish()
        start = time.ticks_ms()
        waited = time.ticks_diff(start, waited)
        delta = self._can_delta(has_red)
        if delta:
            sent = await self._delta_update(black_left, black_right, left_rows, right_rows)
            print(
                "epd: delta sent in", time.ticks_diff(sent, start),
                "ms after waiting", waited, "ms for the previous refresh",
            )
        else:
            sent = await self._full_update(black_left, red_left, black_right, red_right, has_red)
            print(
                "epd: full sent in", time.ticks_diff(sent, start),
                "ms, refreshed in", time.ticks_diff(time.ticks_ms(), sent), "ms",
            )
        return delta

    def _can_delta(self, has_red: bool) -> bool:
        """Delta needs the film's exact contents, and its waveform can't move red pigment."""
        return (
            self._driver_b and self._film_ok and not has_red and not self._screen_red
            and self._delta_frames < _CLEAN_EVERY
        )

    def delta_frames_left(self) -> int:
        """Delta frames before one goes through the full ~6 s waveform. 0 if the next one will."""
        if not (self._driver_b and self._film_ok and not self._screen_red):
            return 0
        return max(0, _CLEAN_EVERY - self._delta_frames)

    def request_clean(self):
        """Send the next frame through the full waveform, so a run of delta frames can start fresh."""
        self._film_ok = False

    async def _full_update(self, black_left, red_left, black_right, red_right, has_red: bool):
        await self._close_session()
        await self._reset()
        self._send_images(black_left, red_left, black_right, red_right)
        sent = time.ticks_ms()
        await self.cog_initial(self._waveform(has_red))
        await self._dcdc_soft_start()
        await self._display_refresh(has_red)
        await self._dcdc_soft_shutdown()
        self._screen_red = has_red
        self._delta_frames = 0
        self._film_ok = self._driver_b and not has_red
        if self._film_ok:
            self._film[0][:] = black_left
            self._film[1][:] = black_right
        return sent

    async def _delta_update(self, black_left, black_right, left_rows, right_rows):
        """Redraw only the pixels that changed: the previous frame goes in the red plane.

        Returns once the refresh has started, so the next frame can be drawn while the panel
        runs it. Caller must have awaited _finish(): planes sent while the panel is busy are dropped.
        """
        film_left, film_right = self._film
        fast = self.fast_frames
        if self._session_frames >= self.session_max_frames or (self._hot and fast != self._hot_fast):
            await self._close_session()
        if self._hot:
            # The CoG swaps 0x10 and 0x11 after a pull, so 0x11 already holds the film and 0x10
            # only differs from the new frame in the rows of this change and the last one.
            prev_left, prev_right = self._prev_rows
            self._send_rows(self._send_index_data_left, black_left, _union_rows(left_rows, prev_left))
            self._send_rows(self._send_index_data_right, black_right, _union_rows(right_rows, prev_right))
            # The pull scans the DUW window, so it has to cover the whole panel again.
            self._send_index_data(0x13, [0x00, 0x3B, 0x00, 0x00, 0x9F, 0x02])
            sent = time.ticks_ms()
        else:
            await self._reset()
            self._send_images(black_left, film_left, black_right, film_right)
            sent = time.ticks_ms()
            await self.cog_initial(luts.LUTB_FAST if fast else luts.LUTB_DELTA)
            await self._dcdc_soft_start()
            self._hot = True
            self._hot_fast = fast
            self._session_frames = 0
        self._prev_rows = (left_rows, right_rows)
        self._send_index_data(0x15, [0x3C])
        await self._wait_busy(50)
        self._inflight = True
        film_left[:] = black_left
        film_right[:] = black_right
        self._session_frames += 1
        self._delta_frames += 1
        self._last_frame = time.ticks_ms()
        return sent

    def _send_rows(self, send, plane, rows: int):
        """Write rows of a half plane to one CoG's 0x10 RAM. send picks the CoG."""
        if rows < 0:
            return
        first, last = rows >> 10, rows & 0x3FF
        send(0x13, [0x00, 0x3B, first & 0xFF, first >> 8, last & 0xFF, last >> 8])
        send(0x12, [0x3B, first & 0xFF, 0x14 | first >> 8])
        send(0x10, memoryview(plane)[first * display_half_width_bytes : (last + 1) * display_half_width_bytes])

    async def _finish(self):
        """Wait for a refresh a delta frame left running. A hung one resets the controller."""
        if not self._inflight:
            return
        self._inflight = False
        if not await self._wait_ready(_DELTA_TIMEOUT_MS):
            self._abort()

    async def _close_session(self):
        await self._finish()
        if self._hot:
            self._hot = False
            await self._dcdc_soft_shutdown()

    def _abort(self):
        """A refresh never finished: hold the COG in reset and forget what the film shows."""
        EPD_RST_PIN.value(False)
        EPD_CS1_PIN.value(True)
        EPD_CS2_PIN.value(True)
        self._hot = False
        self._film_ok = False
        self._screen_red = True
        print("epd: refresh timed out, controller reset")

    def _waveform(self, has_red: bool) -> bytes | None:
        """None keeps the OTP waveform."""
        if self._driver_b:
            # The fast table leaves red faint at any trigger count; the OTP waveform takes ~34 s.
            return None if has_red else luts.LUTB
        if has_red:
            return luts.LUT8_RED
        if self._screen_red:
            return luts.LUT8_RED_CLEAR
        return luts.LUT8_NORED

    async def cog_initial(self, waveform: bytes | None):
        """Register sequence from documentation/itc97-driver-sheets.pdf, ending with the waveform write.

        Driver 8 waits a quarter of the sheet's times (1210 ms in total there).
        """
        # Driver B latches 0xB0 with far shorter waits. A write that misses shows as a ~33 s pull.
        settle, high, low, load = (1, 2, 10, 10) if self._driver_b else (10, 25, 25, 125)
        self._send_index_data(0x05, [0x7D])
        await aio.sleep_ms(50)
        self._send_index_data(0x05, [0x00])
        await aio.sleep_ms(settle)
        if not self._driver_b:
            self._send_index_data(0xC2, [0x3F])
        self._send_index_data(0xD8, [0x80])  # MS_SYNC
        self._send_index_data(0xD6, [0x00])  # BVSS
        self._send_index_data(0xA7, [0x10])
        await aio.sleep_ms(high)
        self._send_index_data(0xA7, [0x00])
        await aio.sleep_ms(low)
        if not self._driver_b:
            self._send_index_data(0x03, [0x00, 0x11])  # OSC

        self._send_index_data(0x44, [0x00])
        self._send_index_data(0x45, [0x80])
        self._send_index_data(0xA7, [0x10])
        await aio.sleep_ms(high)
        self._send_index_data(0xA7, [0x00])
        await aio.sleep_ms(low)
        self._send_index_data(0x44, [0x06])
        self._send_index_data(0x45, [0x84 if self._driver_b else 0x86])  # bank for the 9.7" panel
        self._send_index_data(0xA7, [0x10])
        await aio.sleep_ms(high)
        self._send_index_data(0xA7, [0x00])
        # This strobe loads the OTP waveform; writing 0xB0 before it has finished gets overwritten.
        await aio.sleep_ms(load)

        self._send_index_data(0x60, [0x25])  # TCON
        self._send_index_data_left(0x61, [0x01])  # STV_DIR for Display 1
        self._send_index_data(0x02, [0x00])  # VCOM
        # The only point where 0xB0 takes effect: after the strobes, before the DC/DC soft start.
        if waveform is not None:
            self._send_index_data(0xB0, waveform)

    async def cog_poweroff(self):
        """CoG shutdown function
        Shuts down the CoG and DC/DC circuit after all update functions
        """
        self._send_index_data(0x09, [0x7F, 0x7D, 0x00])
        await aio.sleep_ms(200)
        await self._wait_ready()
        EPD_DC_PIN.value(False)
        EPD_CS1_PIN.value(False)
        await aio.sleep_ms(150)
        EPD_RST_PIN.value(False)

    def _send_index_data(self, index: int, data: bytes | list):
        EPD_DC_PIN.value(False)
        EPD_CS1_PIN.value(False)
        EPD_CS2_PIN.value(False)
        self.spi.write(bytes([index]))
        EPD_CS1_PIN.value(True)
        EPD_CS2_PIN.value(True)
        EPD_DC_PIN.value(True)
        EPD_CS1_PIN.value(False)
        EPD_CS2_PIN.value(False)
        self.spi.write(_wire(data))
        EPD_CS1_PIN.value(True)
        EPD_CS2_PIN.value(True)

    def _send_index_data_left(self, index: int, data: bytes | list | memoryview[int]):
        EPD_DC_PIN.value(False)
        EPD_CS1_PIN.value(False)
        EPD_CS2_PIN.value(True)
        self.spi.write(bytes([index]))
        EPD_CS1_PIN.value(True)
        EPD_DC_PIN.value(True)
        EPD_CS1_PIN.value(False)
        self._write_chunked(data)
        EPD_CS1_PIN.value(True)

    def _write_chunked(self, data):
        chunks = self._plane_chunks.get(id(data))
        if chunks is None:
            if len(data) <= _CHUNK:
                self.spi.write(_wire(data))
                return
            chunks = _chunks(data)
        for chunk in chunks:
            self.spi.write(chunk)

    def _send_index_data_right(self, index: int, data: bytes | list | memoryview[int]):
        EPD_DC_PIN.value(False)
        EPD_CS1_PIN.value(True)
        EPD_CS2_PIN.value(False)
        self.spi.write(bytes([index]))
        EPD_CS2_PIN.value(True)
        EPD_DC_PIN.value(True)
        EPD_CS2_PIN.value(False)
        self._write_chunked(data)
        EPD_CS2_PIN.value(True)

    async def _wait_ready(self, timeout_ms: int = 180_000):
        """Wait for the panel to finish. False if it was still busy at the timeout."""
        start = time.ticks_ms()
        while _busy():
            if time.ticks_diff(time.ticks_ms(), start) >= timeout_ms:
                return False
            await aio.sleep_ms(1)
        return True

    async def _wait_busy(self, timeout_ms: int):
        """Wait for the panel to report busy. False if it never did."""
        start = time.ticks_ms()
        while not _busy():
            if time.ticks_diff(time.ticks_ms(), start) >= timeout_ms:
                return False
            await aio.sleep_ms(1)
        return True

    async def _display_refresh(self, has_red: bool):
        await self._wait_ready(10_000)
        # Driver B plays only a prefix of the fast table per trigger, so it takes three. Red frames
        # run the OTP waveform, which needs one.
        pulls = 3 if self._driver_b and not has_red else 1
        for _ in range(pulls):
            self._send_index_data(0x15, [0x3C])
            if await self._wait_busy(2_000):
                await self._wait_ready()
        # Driver 8 can start a second pass on its own shortly after the first ends. Driver B
        # never did in testing, and the watch costs a second per refresh.
        if not self._driver_b and await self._wait_busy(1_000):
            await self._wait_ready()

    async def _reset(self):
        """CoG driver power-on hard reset"""
        high, low, settle = (2, 4, 25) if self._driver_b else (12, 25, 100)
        EPD_RST_PIN.value(True)
        await aio.sleep_ms(high)
        EPD_RST_PIN.value(False)
        await aio.sleep_ms(low)
        EPD_RST_PIN.value(True)
        await aio.sleep_ms(settle)
        EPD_CS1_PIN.value(True)
        EPD_CS2_PIN.value(True)
        if self._driver_b:
            # Booster setting, errata 9 of the iTC 9.7" app note: without it a B1 draws ~2 A per refresh.
            self._send_index_data(0x70, [0x71, 0x36])

    def _send_images(
        self,
        black_left: bytearray | bytes | memoryview[int],
        red_left: bytearray | bytes | memoryview[int],
        black_right: bytearray | bytes | memoryview[int],
        red_right: bytearray | bytes | memoryview[int],
    ):
        assert len(black_left) == half_frame_one_color_size
        assert len(red_left) == half_frame_one_color_size
        assert len(black_right) == half_frame_one_color_size
        assert len(red_right) == half_frame_one_color_size

        # DUW for both left and right halves
        self._send_index_data(0x13, [0x00, 0x3B, 0x00, 0x00, 0x9F, 0x02])
        # DRFW for Both left and right halves
        self._send_index_data(0x90, [0x00, 0x3B, 0x00, 0xA9])
        ram_rw_data = [0x3B, 0x00, 0x14]
        self._send_index_data_left(0x12, ram_rw_data)  # RAM_RW for left half
        self._send_index_data_left(0x10, black_left)  # Black frame for left half
        self._send_index_data_left(0x12, ram_rw_data)  # RAM_RW for left half
        self._send_index_data_left(0x11, red_left)  # Red frame for left half
        self._send_index_data_right(0x12, ram_rw_data)  # RAM_RW for right half
        self._send_index_data_right(0x10, black_right)  # black frame for right half
        self._send_index_data_right(0x12, ram_rw_data)  # RAM_RW for right half
        self._send_index_data_right(0x11, red_right)  # Red frame for right half


    async def _dcdc_soft_start(self):
        """DC-DC soft-start command
        Implemented after image data are uploaded to CoG
        Specific to mid-sized EPDs only
        """
        self._send_index_data(0x51, [0x50, 0x01])

        # Blocking sleeps: asyncio.sleep_ms(2) wakes late and stretched this ramp from ~100 ms to 330 ms.
        for val in range(1, 5):
            self._send_index_data(0x09, [0x1F])
            self._send_index_data(0x51, [0x50, val])
            self._send_index_data(0x09, [0x9F])
            time.sleep_ms(2)

        for val in range(1, 11):
            self._send_index_data(0x09, [0x1F])
            self._send_index_data(0x51, [0x0A, val])
            self._send_index_data(0x09, [0x9F])
            time.sleep_ms(2)

        for val in range(3, 11):
            self._send_index_data(0x09, [0x7F])
            self._send_index_data(0x51, [0x0A, val])
            self._send_index_data(0x09, [0xFF])
            time.sleep_ms(2)

        for val in range(9, 1, -1):
            self._send_index_data(0x09, [0x7F])
            self._send_index_data(0x51, [val, 0x0A])
            self._send_index_data(0x09, [0xFF])
            time.sleep_ms(2)

        self._send_index_data(0x09, [0xFF])
        await aio.sleep_ms(10)

    async def _dcdc_soft_shutdown(self):
        """DC-DC soft-shutdown command
        Implemented after image data are uploaded to CoG
        Specific to mid-sized EPDs only
        """
        await self._wait_ready()
        self._send_index_data(0x09, [0x7F])
        await aio.sleep_ms(20)
        self._send_index_data(0x05, [0x7D])
        self._send_index_data(0x09, [0x00])
        await aio.sleep_ms(50)
        await self._wait_ready()
        EPD_DC_PIN.value(False)
        EPD_CS1_PIN.value(False)
        EPD_RST_PIN.value(False)
        EPD_CS1_PIN.value(True)


display = EPaperDisplay()
