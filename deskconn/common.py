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
import sys
import time

from autobahn.twisted.component import Component
from autobahn.wamp.auth import AuthCryptoSign

from deskconnd.database.controller import DB

PREFIX = "org.deskconn.deskconn.{component}."


def is_snap():
    return os.environ.get("SNAP_NAME") == "deskconn"


def wait_for_deskconnd():
    if is_snap():
        crossbar = os.path.expandvars("$SNAP_COMMON/runtime/bin/crossbar")
        if not os.path.exists(crossbar):
            print("Waiting for deskconnd runtime directory interface to connect")
            while not os.path.exists(crossbar):
                time.sleep(1)
        print("Found deskconnd runtime environment")

        ready_file = os.path.join(os.path.expandvars('$SNAP_COMMON/state'), 'ready')
        print("finding deskconnd...")
        while not os.path.exists(ready_file):
            time.sleep(1)
        print("found, now connecting.")


def get_component():
    principle = DB.get_local_principle()
    if not principle:
        print("The backend is likely not running, please ensure its up.")
        sys.exit(1)
    return Component(transports="ws://localhost:5020/ws", realm=principle.realm,
                     authentication={"cryptosign": AuthCryptoSign(authid=principle.auth_id,
                                                                  authrole=principle.auth_role,
                                                                  privkey=principle.private_key,
                                                                  authextra={})})
