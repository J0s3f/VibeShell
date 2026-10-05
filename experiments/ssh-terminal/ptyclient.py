#!/usr/bin/env python3
"""Drive the real ssh(1) client on a real pseudo-terminal.

The Go clients in the spike tests can ask for whatever byte stream they like.
A person cannot: ssh(1) puts the local terminal in raw mode, decides on its own
whether to allocate a pty, and turns a local window change into an SSH
window-change request. This driver makes those decisions happen for real, so the
receipts record the server's view of a genuine client.

The sequence is fixed and printed as it runs:

  1. start ssh -tt on a pty of a known size,
  2. read the session's welcome line,
  3. send an escape sequence one byte at a time, so the server sees a key split
     across SSH packets,
  4. resize the pty, which makes ssh send window-change,
  5. send 0x03, which a pty session delivers in band rather than as a signal,
  6. type a line the session ends on, so the exit status is the server's,
  7. close the pty master, which makes the client's input end,

and finally reports the client's exit status. Text arguments accept \\r, \\n,
\\t, \\e, and \\xNN escapes, so a carriage return reaches the pty as one byte
rather than as two characters. Everything the server logged is printed too when
--server-log is given.
"""

import argparse
import fcntl
import os
import pty
import select
import struct
import subprocess
import sys
import termios
import time

QUIET = 0.5  # seconds of silence that mean "nothing more is coming"


def set_window_size(fd, rows, cols):
    """Resize a pty, which delivers SIGWINCH to its foreground process group."""
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def unescape(text):
    """Turn the escape sequences in a command-line argument into real bytes.

    A carriage return or an escape byte has to reach the pty as the byte itself,
    and passing it through two shells and a shell-quoted argument is where that
    usually goes wrong. Interpreting it here keeps the harness readable.
    """
    result = bytearray()
    index = 0
    while index < len(text):
        char = text[index]
        if char != "\\" or index + 1 == len(text):
            result.extend(char.encode())
            index += 1
            continue
        following = text[index + 1]
        if following == "x" and index + 3 < len(text):
            result.append(int(text[index + 2:index + 4], 16))
            index += 4
            continue
        simple = {"r": "\r", "n": "\n", "t": "\t", "e": "\x1b", "0": "\0", "\\": "\\"}
        if following in simple:
            result.extend(simple[following].encode())
            index += 2
            continue
        result.extend(char.encode())
        index += 1
    return bytes(result)


def claim_controlling_terminal(slave_fd):
    """Make the new session's pty its controlling terminal.

    A terminal emulator does this for every program it starts, and ssh depends
    on it: its SIGWINCH handler only runs when the pty is the process
    group's controlling terminal, and that handler is what turns a local resize
    into an SSH window-change request.
    """
    os.setsid()
    fcntl.ioctl(slave_fd, termios.TIOCSCTTY, 0)


def read_available(fd, deadline):
    """Read whatever the pty has until it goes quiet or the deadline passes."""
    chunks = []
    while time.monotonic() < deadline:
        remaining = max(0.0, min(QUIET, deadline - time.monotonic()))
        readable, _, _ = select.select([fd], [], [], remaining)
        if not readable:
            if chunks:
                break
            continue
        try:
            data = os.read(fd, 65536)
        except OSError:
            break
        if not data:
            break
        chunks.append(data)
    return b"".join(chunks)


def show(label, data):
    text = data.decode("utf-8", "replace")
    printable = text.replace("\r\n", "\\r\\n").replace("\r", "\\r").replace("\x1b", "\\x1b")
    print(f"[pty] {label}: {printable}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=2222)
    parser.add_argument("--user", default="ada")
    parser.add_argument("--term", default="xterm-256color")
    parser.add_argument("--initial", default="24x80", help="initial ROWSxCOLS")
    parser.add_argument("--resize-to", default="40x132", help="ROWSxCOLS after the first read")
    parser.add_argument("--text", default="", help="text to type before the resize")
    parser.add_argument("--fragment", default="", help="key sequence to send one byte at a time")
    parser.add_argument("--send-hex", default="", help="hex bytes to send after the resize, e.g. 03")
    parser.add_argument("--send-text", default="",
                        help="text to type after the raw bytes, e.g. 'exit 0' with a carriage return")
    parser.add_argument("--server-log-since", default="",
                        help="only report server log lines newer than this marker")
    parser.add_argument("--timeout", type=float, default=60.0)
    parser.add_argument("--server-log", default="", help="path of the server log inside the container")
    args = parser.parse_args()

    initial_rows, initial_cols = (int(value) for value in args.initial.lower().split("x"))
    new_rows, new_cols = (int(value) for value in args.resize_to.lower().split("x"))
    deadline = time.monotonic() + args.timeout

    master, slave = pty.openpty()
    set_window_size(master, initial_rows, initial_cols)
    command = [
        "ssh",
        "-v", "-tt",
        "-p", str(args.port),
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        f"{args.user}@{args.host}",
    ]
    print(f"[pty] window {initial_rows}x{initial_cols}")
    print(f"[pty] command {' '.join(command)}")
    environment = dict(os.environ, TERM=args.term)
    client = subprocess.Popen(
        command,
        stdin=slave,
        stdout=slave,
        stderr=slave,
        env=environment,
        # The client has to own the pty as its controlling terminal. Without
        # TIOCSCTTY a window change raises no SIGWINCH in the client's process
        # group, and ssh therefore never sends a window-change request at all:
        # the resize would be silently untested.
        preexec_fn=lambda: claim_controlling_terminal(slave),
        close_fds=True,
    )
    os.close(slave)

    try:
        welcome = read_available(master, min(deadline, time.monotonic() + 10))
        show("welcome", welcome)
        if not welcome:
            print("[pty] FAIL: the client produced no session output")
            return 1

        if args.text:
            payload = unescape(args.text)
            os.write(master, payload)
            print(f"[pty] sent text {payload!r}")
            show("text result", read_available(master, time.monotonic() + 3) or b"")

        if args.fragment:
            payload = unescape(args.fragment)
            for index, byte in enumerate(payload):
                os.write(master, bytes([byte]))
                time.sleep(0.01)
                if index < len(payload) - 1:
                    read_available(master, time.monotonic() + 0.05)
            print(f"[pty] sent {payload!r} one byte at a time")
            show("fragment result", read_available(master, time.monotonic() + 3) or b"")

        set_window_size(master, new_rows, new_cols)
        print(f"[pty] window {new_rows}x{new_cols} (ssh sends window-change on SIGWINCH)")
        resized = read_available(master, time.monotonic() + 4)
        show("resize result", resized)
        if not resized:
            # A missing resize is worth showing rather than hiding: it means the
            # client did not turn SIGWINCH into a window-change request.
            print("[pty] note: no output after the resize")

        if args.send_hex:
            payload = bytes.fromhex(args.send_hex)
            os.write(master, payload)
            print(f"[pty] sent bytes {payload.hex(' ')}")
            show("raw result", read_available(master, time.monotonic() + 3) or b"")

        if args.send_text:
            payload = unescape(args.send_text)
            os.write(master, payload)
            print(f"[pty] sent text {payload!r}")
            show("text result", read_available(master, time.monotonic() + 3) or b"")
    finally:
        # Closing the master is how a client that owns a pty learns that its
        # input ended: the pty read fails with EIO, which ssh turns into an SSH
        # EOF. The exit status still has to come from ssh, so the driver waits
        # for the client rather than assuming one.
        status = 0
        try:
            tail = read_available(master, time.monotonic() + 2)
            show("tail", tail)
        except OSError:
            pass
        try:
            status = client.wait(timeout=10)
        except subprocess.TimeoutExpired:
            print("[pty] the client did not exit within 10s; killing it")
            client.kill()
            status = client.wait()
        os.close(master)

    print(f"[pty] ssh exited with status {status}")
    if args.server_log:
        try:
            with open(args.server_log, "r", encoding="utf-8", errors="replace") as handle:
                print("[pty] server log:")
                print(handle.read().rstrip())
        except OSError as error:
            print(f"[pty] server log unavailable: {error}")
    return status


def subprocess_start(command, slave, environment):
    import subprocess

    return subprocess.Popen(
        command,
        stdin=slave,
        stdout=slave,
        stderr=slave,
        env=environment,
        start_new_session=True,
        close_fds=True,
    )


if __name__ == "__main__":
    sys.exit(main())
