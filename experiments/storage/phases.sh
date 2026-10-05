#!/bin/sh
# Runs one phase of a gate whose proof spans processes.
#
# A phase that has to survive a container replacement cannot be written as one test
# run, so the phase name arrives in the environment and this script runs exactly
# that phase. Everything it prints is appended to receipts/restart-transcript.txt in
# the checkout.
#
# A phase may declare the exit status it expects. The unclean-exit phase ends the
# process without closing the database, and a Go test binary can only do that with a
# non-zero status, so its expected status is 1 rather than 0.
#
# Usage: phases.sh <phase> <go test -run pattern> [expected exit status]

set -u

phase="$1"
tests="$2"
expected="${3:-0}"
module=/workspace/experiments/storage
transcript="$module/receipts/restart-transcript.txt"

mkdir -p "$module/receipts"
{
	echo "### phase $phase at $(date -u +%Y-%m-%dT%H:%M:%SZ) in container $(hostname)"
} >>"$transcript"

cd "$module" || exit 1
SPIKE_PHASE="$phase" go test -count=1 -run "$tests" -v ./... >>"$transcript" 2>&1
status=$?
echo "### phase $phase exit=$status (expected $expected)" >>"$transcript"

tail -n 30 "$transcript"

if [ "$status" -ne "$expected" ]; then
	echo "phase $phase exited $status, expected $expected" >&2
	exit 1
fi
exit 0