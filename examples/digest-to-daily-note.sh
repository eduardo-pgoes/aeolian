#!/bin/sh
# An on_finish script that appends each finished run's digest to today's
# daily note, so what needs you is waiting in the note you already open,
# on whichever machine syncs it. aeolian itself never writes to your notes:
# this script does, and only once you point it at a note.
#
# Off by default. To turn it on:
#   1. copy it somewhere stable and make it executable;
#   2. in $AEOLIAN_HOME/config.json:  "on_finish": ["/abs/path/digest-to-daily-note.sh"]
#   3. give the host (e.g. its systemd unit) the note path as a date(1)
#      format:  AEOLIAN_DIGEST_NOTE=/home/me/vault/daily/%Y-%m-%d.md
#
# A clean run appends one line; a run that needs you appends a short block
# with a checkbox per item. When today's note does not exist yet the run
# is skipped rather than creating it, so your note template still applies;
# `aeolian digest --since 12h --md` catches up on anything skipped.
set -eu

[ -n "${AEOLIAN_DIGEST_NOTE:-}" ] || exit 0

# The host hands over the run's inbox entry as JSON on stdin.
run_id=$(sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p' | head -n 1)
[ -n "$run_id" ] || { echo "no run_id on stdin" >&2; exit 1; }

note=$(date +"$AEOLIAN_DIGEST_NOTE")
if [ ! -f "$note" ]; then
	echo "today's note $note does not exist yet; run $run_id not appended" >&2
	exit 0
fi

block=$("${AEOLIAN_BIN:-aeolian}" digest --md --run "$run_id")
printf '\n%s\n' "$block" >>"$note"
