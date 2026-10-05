#!/bin/sh
# SSH/world/app acceptance suite for the VibeShell service (PLAN 15.5 D04).
#
# It builds the real service binary, starts it on a container-local loopback
# port (no host port is ever published), and drives it with the real OpenSSH
# client. Every check prints PASS or FAIL; the script exits non-zero if any
# check failed, so it is usable as a milestone gate.
#
# What this build cannot cover, stated plainly: no generation path is merged
# yet. The composition root wires a deterministic local fallback engine that
# answers pwd, whoami, uname, and echo from configured identity facts and
# reports every other command as truthfully unavailable. So there is no invented
# program, invented path, generated app, or world mutation to assert here. The
# durable state that does exist is the session and turn journal, and that is
# what the durability checks examine.
#
# Usage:
#   tests/e2e/ssh-acceptance.sh [-work DIR] [-keep]
#
# Must run inside the development container (Go toolchain and an OpenSSH
# client). No host port is used; both listeners bind 127.0.0.1 in the
# container's own network namespace.

set -u

WORK=${E2E_WORKDIR:-/tmp/vibeshell-e2e}
KEEP=0
REPO=$(pwd)

while [ $# -gt 0 ]; do
    case "$1" in
        -work) WORK=$2; shift 2 ;;
        -keep) KEEP=1; shift ;;
        -h|--help) sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "unknown option $1" >&2; exit 2 ;;
    esac
done

# --------------------------------------------------------------------------
# Reporting
# --------------------------------------------------------------------------
CHECKS=0
FAILURES=0

log() { printf '%s\n' "$*"; }

pass() {
    CHECKS=$((CHECKS + 1))
    printf 'PASS  %s\n' "$1"
}

fail() {
    CHECKS=$((CHECKS + 1))
    FAILURES=$((FAILURES + 1))
    printf 'FAIL  %s\n' "$1"
    shift
    for line in "$@"; do
        printf '      %s\n' "$line"
    done
}

# check NAME HAYSTACK NEEDLE asserts that NEEDLE appears.
check() {
    case "$2" in
        *"$3"*) pass "$1" ;;
        *) fail "$1" "expected to contain: $3" "actual: $(printf '%s' "$2" | tr -d '\r' | head -c 600)" ;;
    esac
}

# check_absent NAME HAYSTACK NEEDLE asserts that NEEDLE does not appear.
check_absent() {
    case "$2" in
        *"$3"*) fail "$1" "expected NOT to contain: $3" "actual: $(printf '%s' "$2" | tr -d '\r' | head -c 600)" ;;
        *) pass "$1" ;;
    esac
}

# check_status NAME EXPECTED ACTUAL compares an exit status.
check_status() {
    if [ "$2" = "$3" ]; then
        pass "$1 (exit $3)"
    else
        fail "$1" "expected exit $2, got $3"
    fi
}

SERVICE_PID=""

stop_service() {
    if [ -n "$SERVICE_PID" ] && kill -0 "$SERVICE_PID" 2>/dev/null; then
        kill -TERM "$SERVICE_PID" 2>/dev/null
        waited=0
        while kill -0 "$SERVICE_PID" 2>/dev/null && [ "$waited" -lt 100 ]; do
            sleep 0.1
            waited=$((waited + 1))
        done
        wait "$SERVICE_PID" 2>/dev/null
    fi
    SERVICE_PID=""
}

cleanup() {
    stop_service
    if [ "$KEEP" = 0 ] && [ -z "${E2E_WORKDIR:-}" ]; then
        rm -rf "$WORK"
    elif [ "$KEEP" = 1 ]; then
        log "work directory kept at $WORK"
    fi
}
trap cleanup EXIT INT TERM

# --------------------------------------------------------------------------
# Preconditions and build
# --------------------------------------------------------------------------
log "=== VibeShell SSH acceptance suite ==="
log "repository: $REPO"
log "work directory: $WORK"

if [ ! -f "$REPO/go.mod" ]; then
    log "FAIL  no go.mod in $REPO: run this from the repository root inside the dev container"
    log "=== RESULT: FAIL (missing prerequisite) ==="
    exit 1
fi

for tool in ssh go; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        log "FAIL  required tool is missing: $tool"
        log "=== RESULT: FAIL (missing prerequisite $tool) ==="
        exit 1
    fi
done

log "ssh client: $(ssh -V 2>&1)"
log "go: $(go version)"

rm -rf "$WORK"
mkdir -p "$WORK/bin"
BIN="$WORK/bin/vibeshell"
TOOL="$WORK/bin/e2etool"

log ""
log "--- build ---"
if go build -o "$BIN" ./cmd/vibeshell && go build -o "$TOOL" ./tests/e2e/fixtures; then
    pass "build service binary and acceptance tool"
else
    fail "build service binary and acceptance tool"
    log "=== RESULT: FAIL ($FAILURES of $CHECKS checks failed) ==="
    exit 1
fi

# A fresh random password per run keeps the credential out of source control,
# the receipt, and every log line. It is never printed.
ALICE_PASSWORD=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
BOB_PASSWORD=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
DISABLED_PASSWORD=$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
E2E_ALICE_PASSWORD=$ALICE_PASSWORD
E2E_BOB_PASSWORD=$BOB_PASSWORD
E2E_DISABLED_PASSWORD=$DISABLED_PASSWORD
export E2E_ALICE_PASSWORD E2E_BOB_PASSWORD E2E_DISABLED_PASSWORD

# --------------------------------------------------------------------------
# Service lifecycle helpers
# --------------------------------------------------------------------------
BASE_PORT=${E2E_PORT:-0}
if [ "$BASE_PORT" = 0 ]; then
    BASE_PORT=$((20000 + ($$ % 10000)))
fi
PORT=$BASE_PORT
SECURE_PORT=$((BASE_PORT + 1))

log ""
log "--- configure (public 127.0.0.1:$PORT, secure 127.0.0.1:$SECURE_PORT) ---"
if "$TOOL" prepare -dir "$WORK" -port "$PORT" -secure-port "$SECURE_PORT" -prompt-dir "$REPO/prompts/v1"; then
    pass "prepare strict-JSON configurations and owner-only password file"
else
    fail "prepare strict-JSON configurations and owner-only password file"
    log "=== RESULT: FAIL ($FAILURES of $CHECKS checks failed) ==="
    exit 1
fi

PW_MODE=$(ls -l "$WORK/passwords.json" | cut -c1-10)
if [ "$PW_MODE" = "-rw-------" ]; then
    pass "password file is owner-only ($PW_MODE)"
else
    fail "password file is owner-only" "mode is $PW_MODE, want -rw-------"
fi

log ""
log "--- validate configuration ---"
if "$BIN" validate -config "$WORK/public.json" >"$WORK/validate-public.log" 2>&1; then
    pass "public configuration validates"
else
    fail "public configuration validates" "$(cat "$WORK/validate-public.log")"
fi
if "$BIN" validate -config "$WORK/secure.json" >"$WORK/validate-secure.log" 2>&1; then
    pass "secure configuration validates"
else
    fail "secure configuration validates" "$(cat "$WORK/validate-secure.log")"
fi

# start_service CONFIG LOGFILE waits for the composition root to report a ready
# listener. The listener binds 127.0.0.1 inside this container only.
start_service() {
    config_path=$1
    log_path=$2
    "$BIN" run -config "$config_path" >"$log_path" 2>&1 &
    SERVICE_PID=$!
    waited=0
    while [ "$waited" -lt 150 ]; do
        if ! kill -0 "$SERVICE_PID" 2>/dev/null; then
            log "      the service exited during startup:"
            sed 's/^/      /' "$log_path"
            return 1
        fi
        if grep -q '"msg":"vibeshell ready"' "$log_path" 2>/dev/null; then
            return 0
        fi
        sleep 0.1
        waited=$((waited + 1))
    done
    log "      the service did not report a ready listener within 15s"
    return 1
}

# The scripted client must never block on a prompt or a host key check.
SSH_BASE="ssh -p $PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
SSH_SECURE_BASE="ssh -p $SECURE_PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

# shell_lines INPUT_FILE [SSH_ARGS...] drives one scripted session and prints
# everything the client saw. Input lines end with CR, the byte a real terminal
# produces, and stdin is held open briefly so turn output has time to arrive.
shell_lines() {
    input_file=$1
    shift
    {
        [ -s "$input_file" ] && cat "$input_file"
        sleep 5
    } | timeout 90 $SSH_BASE "$@" alice@127.0.0.1 2>&1
}

# --------------------------------------------------------------------------
# 1. Public "none" authentication, MOTD, prompt
# --------------------------------------------------------------------------
log ""
log "--- public mode: none authentication, MOTD, prompt ---"
if start_service "$WORK/public.json" "$WORK/public.log"; then
    pass "service starts and binds 127.0.0.1:$PORT"
else
    fail "service starts and binds 127.0.0.1:$PORT"
    log "=== RESULT: FAIL ($FAILURES of $CHECKS checks failed) ==="
    exit 1
fi

OUT=$(shell_lines /dev/null -tt)
check "public none auth connects and shows the MOTD" "$OUT" "Welcome to VibeOS"
check "MOTD truthfully reports the unavailable generator" "$OUT" "not reachable right now"
check "shell prompt is the familiar user@host:dir form" "$OUT" 'alice@vibeshell.e2e:~$'

# --------------------------------------------------------------------------
# 2. Identity commands
# --------------------------------------------------------------------------
log ""
log "--- simulated identity commands ---"
printf 'pwd\rwhoami\runame\runame -a\r' >"$WORK/in-identity"
OUT=$(shell_lines "$WORK/in-identity")
check "pwd reports the simulated home directory" "$OUT" "/home/alice"
check "whoami reports the public principal" "$OUT" "alice"
check "uname reports the configured system name" "$OUT" "VibeOS"
check "uname -a reports the simulated kernel identity" "$OUT" "GNU/Hurd-style"

# --------------------------------------------------------------------------
# 3. An invented command must not fabricate success
# --------------------------------------------------------------------------
log ""
log "--- unknown command: truthful unavailable, no fake success ---"
printf 'frobnicate --verbose\rpwd\r' >"$WORK/in-unknown"
OUT=$(shell_lines "$WORK/in-unknown")
check "unknown command reports generation unavailable" "$OUT" "model generation is unavailable"
check "unknown command names the command it refused" "$OUT" "frobnicate"
check_absent "unknown command does not fake a shell result" "$OUT" "command not found"
check "session still answers after an unknown command" "$OUT" "/home/alice"

# --------------------------------------------------------------------------
# 4. PTY and window-change
# --------------------------------------------------------------------------
log ""
log "--- PTY and window-change ---"
if grep -q '"msg":"pty requested"' "$WORK/public.log" 2>/dev/null; then
    pass "the client negotiated a pty (pty-req accepted)"
else
    fail "the client negotiated a pty (pty-req accepted)"
fi

# The OpenSSH command line only emits window-change in response to SIGWINCH, so
# a scripted x/crypto/ssh client is used for this one request. The window size
# is not yet propagated into application state by the wired handler, so the
# check is that the transport accepts the request and the session survives.
log "      (window-change via the scripted x/crypto/ssh client)"
RESIZE_OUT=$("$TOOL" resize -addr "127.0.0.1:$PORT" -user alice -cols 100 -rows 40 2>&1)
RESIZE_STATUS=$?
printf '      %s\n' "$RESIZE_OUT"
check_status "window-change is accepted and the session survives" 0 "$RESIZE_STATUS"

# --------------------------------------------------------------------------
# 5. Refused protocol requests
# --------------------------------------------------------------------------
log ""
log "--- refused exec, subsystem, and forwarding ---"
OUT=$(timeout 60 $SSH_BASE alice@127.0.0.1 /bin/true 2>&1)
EXEC_STATUS=$?
check "exec request is refused by the client" "$OUT" "exec request failed"
check_status "exec refusal exits non-zero" 255 "$EXEC_STATUS"

OUT=$(timeout 60 $SSH_BASE -s alice@127.0.0.1 sftp </dev/null 2>&1)
SUBSYS_STATUS=$?
check "subsystem request is refused by the client" "$OUT" "subsystem request failed"
check_status "subsystem refusal exits non-zero" 255 "$SUBSYS_STATUS"

OUT=$(timeout 60 $SSH_BASE -o ExitOnForwardFailure=yes -R 19911:127.0.0.1:$PORT alice@127.0.0.1 </dev/null 2>&1)
FWD_STATUS=$?
check "remote port forwarding is refused" "$OUT" "remote port forwarding failed"
check_status "forwarding refusal exits non-zero" 255 "$FWD_STATUS"

OUT=$(timeout 60 $SSH_BASE -W 127.0.0.1:$PORT alice@127.0.0.1 </dev/null 2>&1)
check "direct-tcpip channel (ssh -W) is refused" "$OUT" "stdio forwarding failed"
check_absent "no forwarded byte reached the shell" "$OUT" "/home/alice"

if grep -q '"msg":"channel refused","type":"direct-tcpip"' "$WORK/public.log" 2>/dev/null; then
    pass "the server logged the refused direct-tcpip channel"
else
    fail "the server logged the refused direct-tcpip channel"
fi
if grep -q '"msg":"global request refused","type":"tcpip-forward"' "$WORK/public.log" 2>/dev/null; then
    pass "the server logged the refused tcpip-forward request"
else
    fail "the server logged the refused tcpip-forward request"
fi

# --------------------------------------------------------------------------
# 6. Ctrl-C
# --------------------------------------------------------------------------
log ""
log "--- Ctrl-C ---"
printf 'half-typed-line\003pwd\r' >"$WORK/in-ctrlc"
OUT=$(shell_lines "$WORK/in-ctrlc" -tt)
check "Ctrl-C echoes the interrupt" "$OUT" "^C"
check_absent "Ctrl-C discards the pending line instead of running it" "$OUT" "half-typed-line: model generation"
check "session still answers after Ctrl-C" "$OUT" "/home/alice"

# --------------------------------------------------------------------------
# 7. Second session and restart durability
# --------------------------------------------------------------------------
log ""
log "--- second session and restart durability ---"
OUT1=$(shell_lines "$WORK/in-identity")
check "first session answered the identity commands" "$OUT1" "/home/alice"

stop_service

# With the service stopped this is the durable state a restart must find, not
# process memory.
"$TOOL" events -db "$WORK/public.db" >"$WORK/events-first.log" 2>&1
EVENTS_STATUS=$?
check_status "durable event store is readable while the service is stopped" 0 "$EVENTS_STATUS"
log "      $(grep '^summary' "$WORK/events-first.log")"

summary_field() {
    printf '%s' "$2" | sed -n "s/.*$1=\([0-9]*\).*/\1/p"
}

SUMMARY1=$(grep '^summary' "$WORK/events-first.log")
FIRST_EVENTS=$(summary_field events "$SUMMARY1")
FIRST_SESSIONS=$(summary_field sessions "$SUMMARY1")

if [ "${FIRST_EVENTS:-0}" -gt 0 ]; then
    pass "sessions are durably recorded ($FIRST_EVENTS events in $FIRST_SESSIONS sessions)"
else
    fail "sessions are durably recorded" "summary: $SUMMARY1"
fi

if start_service "$WORK/public.json" "$WORK/public-restart.log"; then
    pass "service restarts against the same database"
else
    fail "service restarts against the same database"
fi

OUT2=$(shell_lines "$WORK/in-identity")
check "reconnected session reports the same home directory" "$OUT2" "/home/alice"
check "reconnected session reports the same principal" "$OUT2" "alice"

stop_service
"$TOOL" events -db "$WORK/public.db" >"$WORK/events-second.log" 2>&1
SUMMARY2=$(grep '^summary' "$WORK/events-second.log")
log "      $SUMMARY2"
SECOND_EVENTS=$(summary_field events "$SUMMARY2")
SECOND_SESSIONS=$(summary_field sessions "$SUMMARY2")

if [ "${SECOND_EVENTS:-0}" -ge "${FIRST_EVENTS:-0}" ]; then
    pass "restart preserves the durable record and extends it ($FIRST_EVENTS -> $SECOND_EVENTS events)"
else
    fail "restart preserves the durable record and extends it" "events went backwards: $FIRST_EVENTS -> ${SECOND_EVENTS:-0}"
fi
if [ "${SECOND_SESSIONS:-0}" -gt "${FIRST_SESSIONS:-0}" ]; then
    pass "a reconnect appends a new session rather than a new world ($FIRST_SESSIONS -> $SECOND_SESSIONS sessions)"
else
    fail "a reconnect appends a new session rather than a new world" "sessions: $FIRST_SESSIONS -> ${SECOND_SESSIONS:-0}"
fi

# Every public session of the name alice must map to one derived identity, and
# no two distinct names may collide on one.
IDENTITIES=$(grep '^session ' "$WORK/events-second.log" | sed -n 's/.* user=\([^ ]*\).*/\1/p' | sort -u)
IDENTITY_COUNT=$(printf '%s\n' "$IDENTITIES" | grep -c .)
log "      durable identities across public alice sessions: $(printf '%s' "$IDENTITIES" | tr '\n' ' ')"
if [ "$IDENTITY_COUNT" = 1 ]; then
    pass "every public alice session maps to one durable identity"
else
    fail "every public alice session maps to one durable identity" "identities: $(printf '%s' "$IDENTITIES" | tr '\n' ' ')"
fi

CLOSED_COUNT=$(grep '^session ' "$WORK/events-second.log" | grep -c 'closed=true')
TOTAL_COUNT=$(grep -c '^session ' "$WORK/events-second.log")
if [ "$CLOSED_COUNT" = "$TOTAL_COUNT" ] && [ "$TOTAL_COUNT" -gt 0 ]; then
    pass "every session recorded a durable session.end ($CLOSED_COUNT/$TOTAL_COUNT)"
else
    fail "every session recorded a durable session.end" "$CLOSED_COUNT of $TOTAL_COUNT closed"
fi

# --------------------------------------------------------------------------
# 8. Secure mode
# --------------------------------------------------------------------------
log ""
log "--- secure mode: password authentication ---"
if start_service "$WORK/secure.json" "$WORK/secure.log"; then
    pass "secure-mode service starts on 127.0.0.1:$SECURE_PORT"
else
    fail "secure-mode service starts on 127.0.0.1:$SECURE_PORT"
fi

# askpass feeds the password to the real OpenSSH client without a TTY and
# without sshpass. setsid removes the controlling terminal so the client cannot
# fall back to reading the password from one.
cat >"$WORK/askpass.sh" <<'ASKPASS'
#!/bin/sh
printf '%s\n' "$E2E_TEST_PASSWORD"
ASKPASS
chmod 700 "$WORK/askpass.sh"
SSH_ASKPASS="$WORK/askpass.sh"
SSH_ASKPASS_REQUIRE=force
export SSH_ASKPASS SSH_ASKPASS_REQUIRE

# secure_login USER PASSWORD runs the real client and records SECURE_OUT and
# SECURE_STATUS.
secure_login() {
    E2E_TEST_PASSWORD=$2
    export E2E_TEST_PASSWORD
    SECURE_OUT=$(setsid -w timeout 60 $SSH_SECURE_BASE \
        -o PreferredAuthentications=password -o PubkeyAuthentication=no \
        "$1@127.0.0.1" </dev/null 2>&1)
    SECURE_STATUS=$?
}

secure_login alice "$ALICE_PASSWORD"
check "secure alice authenticates with the correct password" "$SECURE_OUT" 'alice@vibeshell.e2e:~$'

secure_login alice "$BOB_PASSWORD"
check_absent "secure alice cannot authenticate with bob's password" "$SECURE_OUT" 'alice@vibeshell.e2e:~$'
check_status "a wrong password is refused" 255 "$SECURE_STATUS"

secure_login nosuchuser "$ALICE_PASSWORD"
check_absent "an unknown secure user cannot authenticate" "$SECURE_OUT" 'alice@vibeshell.e2e:~$'
check_status "an unknown secure user is refused" 255 "$SECURE_STATUS"

secure_login disabled "$DISABLED_PASSWORD"
check_absent "a disabled secure user cannot authenticate" "$SECURE_OUT" 'alice@vibeshell.e2e:~$'
check_status "a disabled user is refused even with the correct password" 255 "$SECURE_STATUS"

NONE_STATUS=$(timeout 60 $SSH_SECURE_BASE -o PreferredAuthentications=none -o BatchMode=yes alice@127.0.0.1 </dev/null >/dev/null 2>&1; echo $?)
check_status "secure mode refuses the public none method" 255 "$NONE_STATUS"

# Secure and public namespaces must not collide: the same username maps to a
# different durable identity in each mode.
stop_service
"$TOOL" events -db "$WORK/secure.db" >"$WORK/events-secure.log" 2>&1
SECURE_MODE_USER=$(grep '^session ' "$WORK/events-secure.log" | sed -n 's/.* user=\([^ ]*\).*/\1/p' | sort -u | head -1)
PUBLIC_MODE_USER=$(printf '%s' "$IDENTITIES" | head -1)
log "      secure-mode identity: $SECURE_MODE_USER"
log "      public-mode identity: $PUBLIC_MODE_USER"
if [ -n "$SECURE_MODE_USER" ] && [ "$SECURE_MODE_USER" != "$PUBLIC_MODE_USER" ]; then
    pass "secure and public identity namespaces do not collide"
else
    fail "secure and public identity namespaces do not collide" "secure=$SECURE_MODE_USER public=$PUBLIC_MODE_USER"
fi

SECURE_SESSION_COUNT=$(grep -c '^session ' "$WORK/events-secure.log")
SECURE_MODE_COUNT=$(grep '^session ' "$WORK/events-secure.log" | grep -c 'mode=secure')
if [ "$SECURE_SESSION_COUNT" -gt 0 ] && [ "$SECURE_MODE_COUNT" = "$SECURE_SESSION_COUNT" ]; then
    pass "every secure session records the secure auth mode ($SECURE_MODE_COUNT/$SECURE_SESSION_COUNT)"
else
    fail "every secure session records the secure auth mode" \
         "$SECURE_MODE_COUNT of $SECURE_SESSION_COUNT recorded mode=secure"
fi

SECURE_CLOSED=$(grep '^session ' "$WORK/events-secure.log" | grep -c 'closed=true')
if [ "$SECURE_CLOSED" = "$SECURE_SESSION_COUNT" ]; then
    pass "every secure session recorded a durable session.end ($SECURE_CLOSED/$SECURE_SESSION_COUNT)"
else
    fail "every secure session recorded a durable session.end" \
         "$SECURE_CLOSED of $SECURE_SESSION_COUNT closed"
fi

# --------------------------------------------------------------------------
# Result
# --------------------------------------------------------------------------
log ""
log "--- internal status surface (no second public port) ---"
"$BIN" status -config "$WORK/public.json" 2>&1 | sed 's/^/      /'

log ""
log "=== RESULT: $((CHECKS - FAILURES))/$CHECKS checks passed ==="
if [ "$FAILURES" -ne 0 ]; then
    log "=== ACCEPTANCE: FAIL ($FAILURES of $CHECKS checks failed) ==="
    exit 1
fi
log "=== ACCEPTANCE: PASS ==="
exit 0