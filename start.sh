#!/bin/sh
# ARTEX supervising startup script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       Run in foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   Run persistently in background
#   ./start.sh -addr :9000           Extra arguments are passed through to artex as-is
#
# It does exactly one thing: run artex, and after the process exits, decide whether to relaunch
# it based on the exit code.
#
#   0      Stopped normally by the user      -> exit the loop
#   75     Program requested a restart       -> relaunch immediately (user clicked "update" or
#                                                "rollback" in the UI)
#   other  Crash                             -> back off, then relaunch (1->2->4... up to 60s max)
#
# Downloading, SHA256 verification, and swapping binaries are deliberately NOT done here: that
# logic would have to be written twice, once for sh and once for bat, and it's exactly the part
# that must not go wrong -- if a binary that can't run gets swapped in, this script would
# faithfully keep relaunching it, leaving the user to fix it by hand on the machine. So
# verification/swapping all lives in Go instead (the selfupdate package), done by artex itself at
# startup, keeping this script dumb on purpose.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward the stop signal to the artex process itself.
#
# This is required under Docker: docker stop only sends SIGTERM to PID 1 (which is this script),
# not to child processes. Without forwarding, artex never receives the signal, can't shut down
# gracefully, and gets hard-killed by SIGKILL after 10 seconds, cutting off any in-progress task
# midway.
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

	# A signal interrupts wait and makes it return >128. At that point the child process is
	# actually still shutting down gracefully, so we must wait on it again to get its real exit
	# code.
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
			# Update/rollback is ready: after relaunching, artex will complete the swap at
			# startup (see selfupdate.Bootstrap).
			echo "[artex] restart requested (applying new version)..."
			delay=1
			;;
		*)
			echo "[artex] exited abnormally (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
