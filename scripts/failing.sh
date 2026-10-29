#!/usr/bin/env bash
# Shows the non-zero exit path and how a failure status envelope closes a run.
set -u
ESC=$'\033'; R="${ESC}[0m"; RED="${ESC}[31m"; B="${ESC}[1m"; GREY="${ESC}[90m"
printf '%s==>%s running migrations\n' "$B" "$R"
for i in 1 2 3; do printf '  %sapplying%s 00%d_add_index.sql\n' "$GREY" "$R" "$i"; sleep 0.6; done
printf '  %sapplying%s 004_backfill.sql\n' "$GREY" "$R"; sleep 1.0
printf '%serror:%s deadlock detected on relation "events"\n' "$RED" "$R" >&2
printf '%serror:%s migration 004 rolled back\n' "$RED" "$R" >&2
exit 3
