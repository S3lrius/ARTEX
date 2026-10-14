#!/bin/sh
# ARTEX supervisor (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   background
#   ./start.sh -addr :9000           extra arguments are passed through to artex
#
# It does one thing: run artex, then decide from the exit code whether to start it again.
#
#   0      clean user stop         → leave the loop
#   75     program asked to restart → rerun immediately
#   other  crash                    → back off and rerun (1→2→4… up to 60 seconds)
#
# This script does not download or replace the binary.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward stop signals to the artex process.
#
# Required under Docker: docker stop sends SIGTERM only to PID 1 (this script),
# not to the child. Without forwarding, artex never sees the signal, cannot shut
# down cleanly, and is SIGKILL'd after 10 seconds, cutting running tasks in half.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# A signal interrupts wait and makes it return >128. The child is still
	# shutting down; wait once more to collect its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			echo "[artex] restart requested…"
			delay=1
			;;
		*)
			echo "[artex] abnormal exit (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
