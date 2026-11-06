"""Button interface for the Billboard Badge"""

import array
import machine
import asyncio as aio
import time
from micropython import const

## Note: plus/minus mislabeled on the silkscreen!
MENU_MINUS_PIN = 40  # SW5  BUTTON_2  IO40
MENU_PLUS_PIN = 15  # SW6  BUTTON_3  IO15
USER_A_PIN = 39  # SW4  BUTTON_1  IO39
USER_B_PIN = 16  # SW7  BUTTON_4  IO16
BOOT_PIN = 0

UNPRESSED = 0
PRESSED_UNREAD = 1
PRESSED_READ = 2

# Edges closer together than this are contact bounce.
_BOUNCE_US = const(15_000)
_QUEUE_LEN = const(32)
# Press queue filled from pin IRQs: button id and ticks_us at the press. The 1 ms scan loop runs
# late whenever a display frame is being prepared; the IRQ callback runs within a few ms.
_q_id = bytearray(_QUEUE_LEN)
_q_us = array.array("l", [0] * _QUEUE_LEN)
_q_pos = array.array("l", [0, 0])  # read, write


def next_press():
    """Oldest queued press as (button id, ticks_us), or None. Ids: MenuPlus 0, MenuMinus 1, UserA 2, UserB 3."""
    r, w = _q_pos[0], _q_pos[1]
    if r == w:
        return None
    _q_pos[0] = (r + 1) % _QUEUE_LEN
    return _q_id[r], _q_us[r]


def clear_presses():
    _q_pos[0] = _q_pos[1]


class Button:
    def __init__(self, pin: int, name: str, button_id: int = -1):
        raw = machine.Pin(pin, machine.Pin.IN, machine.Pin.PULL_UP)
        self.pin = machine.Signal(raw, invert=True)
        self.name = name
        self.value = False
        self._pressed_state = UNPRESSED
        self._press_onset_ms = None
        if button_id >= 0:
            self._id = button_id
            self._raw = raw
            self._last_edge = time.ticks_us()
            raw.irq(self._edge, machine.Pin.IRQ_FALLING | machine.Pin.IRQ_RISING)

    def _edge(self, _pin):
        now = time.ticks_us()
        quiet = time.ticks_diff(now, self._last_edge) > _BOUNCE_US
        self._last_edge = now
        if quiet and not self._raw.value():
            w = _q_pos[1]
            nxt = (w + 1) % _QUEUE_LEN
            if nxt != _q_pos[0]:
                _q_id[w] = self._id
                _q_us[w] = now
                _q_pos[1] = nxt

    def scan(self):
        self.value = self.pin.value()
        if self.value and self._pressed_state == UNPRESSED:
            self._pressed_state = PRESSED_UNREAD
            self._press_onset_ms = time.ticks_ms()
        if not self.value: # and self._pressed_state == PRESSED_READ:
            self._pressed_state = UNPRESSED
            self._press_onset_ms = None

    def pressed(self) -> bool:
        """Was a button pressed? Only returns True once for each press."""
        if self._pressed_state == PRESSED_UNREAD:
            self._pressed_state = PRESSED_READ
            return True
        return False

    def pressed_ms(self) -> int:
        if self._pressed_state in (PRESSED_UNREAD, PRESSED_READ) and self._press_onset_ms is not None:
            return time.ticks_diff(time.ticks_ms(), self._press_onset_ms)
        return 0

    def __str__(self):
        if self._pressed_state == UNPRESSED:
            pressed_state_name = "unpressed"
        elif self._pressed_state == PRESSED_UNREAD:
            pressed_state_name = "pressed-unread"
        elif self._pressed_state == PRESSED_READ:
            pressed_state_name = "pressed-read"
        else:
            pressed_state_name = ""
        return f"{self.name}:{self.value}:{pressed_state_name}"

MenuPlus = Button(MENU_PLUS_PIN, "Menu +", 0)
MenuMinus = Button(MENU_MINUS_PIN, "Menu -", 1)
UserA = Button(USER_A_PIN, "User A", 2)
UserB = Button(USER_B_PIN, "User B", 3)
Boot = Button(BOOT_PIN, "Boot")


async def scan():
    """Scan buttons and update persisted state"""
    while True:
        MenuMinus.scan()
        MenuPlus.scan()
        UserA.scan()
        UserB.scan()
        Boot.scan()
        await aio.sleep_ms(1)
