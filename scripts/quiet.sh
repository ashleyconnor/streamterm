#!/usr/bin/env bash
# A long silent gap, to exercise SSE heartbeats and idle-proxy survival.
set -u
ESC=$'\033'; R="${ESC}[0m"; B="${ESC}[1m"; D="${ESC}[2m"
printf '%s==>%s waiting on a slow external service\n' "$B" "$R"
printf '   %s(20 s of silence — the stream stays open on comment frames)%s\n' "$D" "$R"
sleep 20
printf '%s==>%s service responded\n' "$B" "$R"
