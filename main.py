#!/usr/bin/env python3
"""Launch the statically linked forge binary beside this file.

The trial runs `python3 /bot/main.py`; the binary does the whole job:
it binds an ephemeral port, registers with the environment, serves the
forge, polls for completion and exits 0. Nothing is written anywhere.
"""

import os
import sys


def main() -> int:
    here = os.path.dirname(os.path.abspath(__file__))
    exe = os.path.join(here, "forge")
    if not os.path.isfile(exe):
        sys.stderr.write("forge binary missing beside main.py\n")
        return 1
    os.execv(exe, [exe])
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
