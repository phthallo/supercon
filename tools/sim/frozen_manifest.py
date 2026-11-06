# Extra frozen modules for the simulator's Unix build of lvgl_micropython.
# Its builder replaces the Unix port's default manifest, which drops asyncio
# (and the uasyncio alias the firmware imports). The badge's ESP32 build has both.
include("$(MPY_DIR)/extmod/asyncio")
