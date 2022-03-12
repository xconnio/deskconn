#!/usr/bin/env python3
#
# Copyright (c) CODEBASE
#
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
#
# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.
#

import time

from autobahn import wamp
from evdev import uinput, ecodes


class Slides:
    def __init__(self):
        self.device = uinput.UInput()

    def _press_and_release(self, key):
        self.device.write(ecodes.EV_KEY, key, 1)
        time.sleep(0.1)
        self.device.write(ecodes.EV_KEY, key, 0)
        self.device.syn()

    @wamp.register(None)
    def next(self):
        self._press_and_release(ecodes.KEY_PAGEDOWN)

    @wamp.register(None)
    def previous(self):
        self._press_and_release(ecodes.KEY_PAGEUP)

    @wamp.register(None)
    def start(self):
        self._press_and_release(ecodes.KEY_F5)

    @wamp.register(None)
    def end(self):
        self._press_and_release(ecodes.KEY_ESC)
