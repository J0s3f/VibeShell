#!/usr/bin/env python3
"""Check what a real OpenSSH client gets when it uses a local port forward.

`ssh -L` only opens a listener on the client side; the SSH direct-tcpip channel
arrives when something connects to that port. A server that refuses forwarding
therefore refuses the channel, not the listener, so the check has to use the
forward to see the refusal.

The script starts the forward in the background, connects to it, prints whatever
comes back, and then stops the client. It exists so the driving logic stays out
of run-spike.ps1's quoting.
"""

import argparse
import os
import signal
import socket
import subprocess
import sys
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=2222)
    parser.add_argument("--user", default="ada")
    parser.add_argument("--listen-port", type=int, default=19999)
    parser.add_argument("--target-port", type=int, default=19998)
    parser.add_argument("--timeout", type=float, default=25.0)
    args = parser.parse_args()

    command = [
        "ssh", "-f", "-N",
        "-L", f"{args.listen_port}:127.0.0.1:{args.target_port}",
        "-p", str(args.port),
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "LogLevel=ERROR",
        f"{args.user}@{args.host}",
    ]
    print(f"[forward] {' '.join(command)}")
    deadline = time.monotonic() + args.timeout
    client = subprocess.Popen(command, start_new_session=True,
                              stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    try:
        # -f backgrounds the client after authentication, so the process may
        # exit while the listener lives on; wait for the port instead.
        while time.monotonic() < deadline:
            try:
                probe = socket.create_connection(("127.0.0.1", args.listen_port), 0.5)
                probe.close()
                break
            except OSError:
                if client.poll() is not None:
                    print(f"[forward] ssh exited early with {client.returncode}")
                    return 1
                time.sleep(0.2)
        else:
            print("[forward] the local listener never opened")
            return 1

        print("[forward] local listener accepted a connection")
        try:
            used = socket.create_connection(("127.0.0.1", args.listen_port), 5)
        except OSError as error:
            print(f"[forward] could not connect to the forwarded port: {error}")
            return 1
        used.settimeout(5)
        used.sendall(b"probe\n")
        try:
            received = used.recv(4096)
        except OSError as error:
            print(f"[forward] read error after connecting: {error}")
            received = b""
        print(f"[forward] read {len(received)} bytes: {received!r}")
        used.close()
        if not received:
            # A refused channel is how a server declines forwarding: the local
            # connection is closed with no data, and the reason reaches the
            # client on its own stderr. run-spike.ps1 asserts the reason from
            # the server log, which is where it is unambiguous.
            print("[forward] the forward was closed without data, which is how a "
                  "refused direct-tcpip channel reaches a client")
            return 0
        print("[forward] unexpected: the forward delivered data")
        return 1
    finally:
        # Stop the backgrounded client; the listener dies with it.
        try:
            os.killpg(os.getpgid(client.pid), signal.SIGTERM)
        except (OSError, ProcessLookupError):
            pass
        try:
            client.wait(timeout=5)
        except subprocess.TimeoutExpired:
            client.kill()


if __name__ == "__main__":
    sys.exit(main())
