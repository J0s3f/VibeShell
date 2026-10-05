#!/usr/bin/env python3
"""Print a password from a file, for ssh(1)'s SSH_ASKPASS.

A real ssh client asks for a password on a terminal or through SSH_ASKPASS. The
spike has no terminal, so it uses this helper: ssh runs it with no arguments and
expects the password on stdout. The path comes from ASKPASS_FILE in the
environment, which ssh passes on to the programs it starts, so no password ever
appears in a command line, in a receipt, or in this file.
"""

import os
import sys

PATH_VARIABLE = "ASKPASS_FILE"


def main():
    path = os.environ.get(PATH_VARIABLE)
    if not path:
        print(f"{PATH_VARIABLE} is not set", file=sys.stderr)
        return 1
    try:
        with open(path, "r", encoding="utf-8") as handle:
            sys.stdout.write(handle.read().strip() + "\n")
    except OSError as error:
        print(f"cannot read {path}: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
