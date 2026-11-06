"""Starter app. Copy firmware/apps/template.py for the full button example."""

import lvgl

from apps.base_app import BaseApp
from lib.button import UserA, UserB
from lib.epd import display, display_width, display_height
from lib.led_bar import leds

_TEXT = "Hello, badge!"


class HelloApp(BaseApp):
    def __init__(self) -> None:
        super().__init__("Hello")

    def run(self):
        if self.launched:
            self._draw()
        elif UserA.pressed() or UserB.pressed():
            self._draw()

    def _draw(self):
        leds.fill((0, 1, 1))
        scr = lvgl.obj()
        scr.remove_style_all()
        scr.set_size(display_width, display_height)
        scr.set_style_bg_color(lvgl.color_hex(0xFFFFFF), 0)
        scr.set_style_bg_opa(lvgl.OPA.COVER, 0)

        label = lvgl.label(scr)
        label.set_text(_TEXT)
        label.set_style_text_color(lvgl.color_hex(0xFF0000), 0)
        label.set_style_text_font(lvgl.font_montserrat_48, 0)
        label.center()

        display.clear()
        lvgl.screen_load(scr)
        scr.update_layout()
        lvgl.refr_now(display.lv_display)
        display.update()
        leds.fill((0, 1, 0))
