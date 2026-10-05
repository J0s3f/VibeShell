#!/usr/bin/env python3
"""Stand in for a running ssh-agent so `ssh -A` really sends auth-agent-req.

ssh only offers agent forwarding when SSH_AUTH_SOCK names a socket it can
connect to, so the refusal path cannot be exercised without one. It also asks
the agent which identities it holds before it offers forwarding, so a stand-in
has to answer that request rather than merely accept the connection: request 11
(SSH_AGENTC_REQUEST_IDENTITIES) is answered with 12 (IDENTITIES_ANSWER) and a
count of zero.

The helper then runs the command it is given with that socket in the
environment, and exits with the command's status. It never holds a key: the
point is to make the client offer agent forwarding, not to authenticate anything.
"""

import os
import socket
import struct
import subprocess
import sys
import threading

PATH = "/tmp/fake-agent.sock"
REQUEST_IDENTITIES = 11
IDENTITIES_ANSWER = 12
ANSWER_LENGTH = 5  # one type byte plus a four-byte count of zero


def answer_one(connection):
    """Serve one identity request, then close: that is all ssh needs."""
    try:
        header = connection.recv(4)
        if len(header) != 4:
            return
        length = struct.unpack(">I", header)[0]
        request = connection.recv(length) if length else b""
        if not request or request[0] != REQUEST_IDENTITIES:
            return
        connection.sendall(struct.pack(">IBI", ANSWER_LENGTH, IDENTITIES_ANSWER, 0))
    except OSError:
        pass
    finally:
        connection.close()


def serve(listener):
    while True:
        try:
            connection, _ = listener.accept()
        except OSError:
            return
        threading.Thread(target=answer_one, args=(connection,), daemon=True).start()


def main():
    if os.path.exists(PATH):
        os.unlink(PATH)
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    listener.bind(PATH)
    listener.listen(4)
    threading.Thread(target=serve, args=(listener,), daemon=True).start()

    environment = dict(os.environ, SSH_AUTH_SOCK=PATH)
    try:
        result = subprocess.run(sys.argv[1:], env=environment, capture_output=True,
                                text=True, timeout=30)
    finally:
        listener.close()
        if os.path.exists(PATH):
            os.unlink(PATH)
    sys.stdout.write(result.stdout)
    sys.stderr.write(result.stderr)
    return result.returncode


if __name__ == "__main__":
    sys.exit(main())
