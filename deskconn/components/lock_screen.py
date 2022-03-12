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

import os

from autobahn import wamp
import dbus


DBUS_DATA = {
    'kde': {
        'service_name': 'org.kde.screensaver',
        'path': '/ScreenSaver',
        'interface': 'org.freedesktop.ScreenSaver',
        'methods': {
            'is_locked': 'GetActive',
            'lock': 'Lock'
        }
    },
    'unity': {
        'service_name': 'org.gnome.ScreenSaver',
        'path': '/com/canonical/Unity/Session',
        'interface': 'com.canonical.Unity.Session',
        'methods': {
            'is_locked': 'IsLocked',
            'lock': 'Lock'
        }
    },
    'gnome': {
        'service_name': 'org.gnome.ScreenSaver',
        'path': '/org/gnome/ScreenSaver',
        'interface': 'org.gnome.ScreenSaver',
        'methods': {
            'is_locked': 'GetActive',
            'lock': 'Lock'
        }
    }
}

DBUS_DATA.update({'ubuntu:gnome': DBUS_DATA['gnome']})
DBUS_DATA.update({'ubuntu:unity': DBUS_DATA['unity']})


class Display:
    def __init__(self):
        self.environment = os.environ.get('XDG_CURRENT_DESKTOP', 'KDE').lower()
        if self.environment not in DBUS_DATA.keys():
            raise RuntimeError('Supported environments: {}'.format(', '.join(DBUS_DATA.keys())))
        bus = dbus.SessionBus()
        self.screen_saver = bus.get_object(DBUS_DATA[self.environment]['service_name'],
                                           DBUS_DATA[self.environment]['path'])
        self.iface = dbus.Interface(self.screen_saver, DBUS_DATA[self.environment]['interface'])

    @wamp.register(None)
    def is_locked(self):
        return getattr(self.iface, DBUS_DATA[self.environment]['methods']['is_locked'])()

    @wamp.register(None)
    def lock(self):
        if not self.is_locked():
            getattr(self.iface, DBUS_DATA[self.environment]['methods']['lock'])()
        return self.is_locked()
