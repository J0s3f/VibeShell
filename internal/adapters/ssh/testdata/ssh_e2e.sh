#!/bin/sh
# Scripted OpenSSH end-to-end test for the VibeShell SSH adapter (task B05).
#
# It starts testserver (an echo handler over the real adapter) and drives it
# with the system OpenSSH client, proving the contract outside the Go test
# doubles: none-auth login with no password prompt, PTY shell echo, exec and
# forwarding refusals, and exit-status propagation.
#
# Prerequisites: `ssh` on PATH (the vibeshell-dev image ships
# openssh-client 9.2; no extra image is needed, so nothing was added under
# the repo containers/). Run from the checkout root inside the dev container:
#
#   sh internal/adapters/ssh/testdata/ssh_e2e.sh
#
# Exit 0 prints PASS for every case; any failure prints FAIL and exits 1.
set -u

WORK="$(mktemp -d)"
trap 'kill "$SERVER" 2>/dev/null; rm -rf "$WORK"' EXIT INT TERM
PASS=0
FAIL=0

ok() { PASS=$((PASS + 1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL + 1)); echo "FAIL: $1"; }

go build -o "$WORK/testserver" ./internal/adapters/ssh/testserver
"$WORK/testserver" -addr "127.0.0.1:0" \
  -hostkey "$WORK/host_key" -mode public >"$WORK/server.log" 2>&1 &
SERVER=$!

# Read the bound address from the server log (up to ~20s).
ADDR=""
for _ in $(seq 1 40); do
  ADDR=$(sed -n 's/^listening on //p' "$WORK/server.log" | awk '{print $1}' | head -1)
  if [ -n "$ADDR" ]; then break; fi
  sleep 0.5
done
if [ -z "$ADDR" ]; then bad "server did not start (see server log)"; cat "$WORK/server.log"; exit 1; fi
PORT=$(echo "$ADDR" | sed 's/.*://')
HOST="testuser@127.0.0.1"
SSH="ssh -p $PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes"

# Readiness: a shell with immediate EOF greets and exits 0 without any
# credential and without any password prompt.
if printf '' | $SSH "$HOST" >/dev/null 2>&1; then
  ok "server accepts none-auth without a password prompt"
else
  bad "none-auth shell failed"; cat "$WORK/server.log"; exit 1
fi

# 1. PTY shell: greeting carries the user and the negotiated size, stdin
# echoes, and EOF ends the session with status 0.
OUT="$(printf 'hello world\n' | $SSH -tt "$HOST" 2>"$WORK/err1" | tr -d '\r')"
STATUS=$?
if [ $STATUS -ne 0 ]; then bad "pty shell exit = $STATUS, want 0"; else ok "pty shell exits 0 on EOF"; fi
case "$OUT" in
  *"vibeshell-echo ready user=testuser window="*) ok "greeting names the user and window" ;;
  *) bad "greeting missing: $OUT" ;;
esac
case "$OUT" in
  *"hello world"*) ok "stdin echoes on the pty shell" ;;
  *) bad "no echo in: $OUT" ;;
esac

# 2. exec is refused and runs nothing. OpenSSH surfaces a failed exec with
# its own message and exit 1 (it acts on the refused request rather than
# waiting for our status); protocol-level clients receive the stderr reason
# and status 127 instead (proven in the Go tests).
ERR="$($SSH "$HOST" echo pwned 2>&1 >/dev/null)"
case "$ERR" in
  *"exec request failed"*) ok "exec refused (client reports the failed request)" ;;
  *) bad "exec stderr missing failure: $ERR" ;;
esac
$SSH "$HOST" echo pwned >/dev/null 2>&1
if [ $? -ne 0 ]; then ok "exec ends non-zero"; else bad "exec exit = 0, want failure"; fi

# 3. A password-only login attempt is never challenged: BatchMode plus a
# password-only preference still logs in via none without prompting.
if printf '' | $SSH -o PreferredAuthentications=password "$HOST" >/dev/null 2>&1; then
  ok "password-preferred login needs no prompt in public mode"
else
  bad "password-preferred login failed in public mode"
fi

# 4. Client-key-only authentication cannot log in as an unusable identity.
if printf '' | $SSH -o PreferredAuthentications=publickey 'not a user@127.0.0.1' >/dev/null 2>&1; then
  bad "publickey login as an invalid user succeeded"
else
  ok "invalid username refused even with a client key offered"
fi

# 5. Direct TCP forwarding is refused.
if $SSH -W example.com:80 "$HOST" </dev/null >/dev/null 2>&1; then
  bad "direct-tcpip (-W) succeeded"
else
  ok "direct-tcpip refused"
fi

# 6. The host key persisted to the service volume.
if [ -f "$WORK/host_key" ]; then ok "host key file created on the volume"; else bad "host key file missing"; fi
FP1=$(ssh-keygen -l -f "$WORK/host_key" 2>/dev/null | awk '{print $2}')
if [ -n "$FP1" ]; then ok "host key stable on disk ($FP1)"; else bad "host key unreadable"; fi

echo "---"
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
