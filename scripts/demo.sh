#!/usr/bin/env bash
#
# A stand-in for a CI job. Each section exercises something that the naive
# "convert ANSI colors to spans" approach gets wrong, so the output is a
# working test of the rendering path as much as it is a demo.

set -u

ESC=$'\033'
RESET="${ESC}[0m"
BOLD="${ESC}[1m"
DIM="${ESC}[2m"
UNDER="${ESC}[4m"
RED="${ESC}[31m"
GREEN="${ESC}[32m"
YELLOW="${ESC}[33m"
BLUE="${ESC}[34m"
MAGENTA="${ESC}[35m"
CYAN="${ESC}[36m"
GREY="${ESC}[90m"

step() { printf '%s==>%s %s%s%s\n' "$BOLD$BLUE" "$RESET" "$BOLD" "$1" "$RESET"; }

# ---------------------------------------------------------------------------
step "Environment as the script sees it"
printf '%sTERM%s          %s\n' "$GREY" "$RESET" "${TERM:-<unset>}"
printf '%sFORCE_COLOR%s   %s\n' "$GREY" "$RESET" "${FORCE_COLOR:-<unset>}"
printf '%sCOLUMNS%s       %s\n' "$GREY" "$RESET" "${COLUMNS:-<unset>}"
if [ -t 1 ]; then
  printf '%sstdout%s        a terminal\n' "$GREY" "$RESET"
else
  printf '%sstdout%s        a pipe %s(no PTY — color is coming from FORCE_COLOR)%s\n' \
    "$GREY" "$RESET" "$DIM" "$RESET"
fi
echo

# ---------------------------------------------------------------------------
step "SGR attributes spanning several writes"
# Each printf is a separate write() and may land in a different network chunk.
# The style opened here has to survive the boundary, which is only true because
# parser state lives in a long-lived Screen rather than being rebuilt per chunk.
printf '%sthis style is opened here' "$MAGENTA$BOLD"
sleep 0.5
printf '  ... continues across a flush boundary'
sleep 0.5
printf '  ... and closes here%s\n' "$RESET"

printf '%sred%s %sgreen%s %syellow%s %scyan%s %sdim%s %sunderlined%s\n' \
  "$RED" "$RESET" "$GREEN" "$RESET" "$YELLOW" "$RESET" \
  "$CYAN" "$RESET" "$DIM" "$RESET" "$UNDER" "$RESET"

printf '256-colour ramp  '
for i in 196 202 208 214 220 226 190 154 118 82 46 47 48 49 50 51; do
  printf '%s██%s' "${ESC}[38;5;${i}m" "$RESET"
done
echo
echo

# ---------------------------------------------------------------------------
step "Carriage-return progress bar"
# The single most common thing that breaks naive log viewers. This writes one
# logical line about forty times, each time returning to column zero. A viewer
# that treats \r as a line break shows forty lines of garbage; one that runs a
# real screen emulator shows a single line that ends at 100%.
width=36
for i in $(seq 0 40); do
  pct=$(( i * 100 / 40 ))
  filled=$(( i * width / 40 ))
  bar=""
  for ((b = 0; b < filled; b++)); do bar+="█"; done
  for ((b = filled; b < width; b++)); do bar+="·"; done
  printf '\r  %sfetching%s [%s%s%s] %3d%%' "$CYAN" "$RESET" "$GREEN" "$bar" "$RESET" "$pct"
  sleep 0.1
done
printf '\r  %sfetching%s [%s%s%s] %sdone%s      \n\n' \
  "$CYAN" "$RESET" "$GREEN" "$(printf '█%.0s' $(seq 1 $width))" "$RESET" "$GREEN" "$RESET"

# ---------------------------------------------------------------------------
step "Spinner overwriting itself in place"
frames='⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏'
for round in 1 2; do
  for f in $frames; do
    printf '\r  %s%s%s resolving dependencies' "$YELLOW" "$f" "$RESET"
    sleep 0.09
  done
done
printf '\r  %s✓%s resolved 143 packages          \n\n' "$GREEN" "$RESET"

# ---------------------------------------------------------------------------
step "stdout and stderr interleaved"
# Both streams share one file descriptor, so ordering is settled by the kernel
# at write time. Two separate pipes read by two goroutines would race and the
# warnings would drift away from the lines they belong to.
for i in 1 2 3 4 5 6; do
  printf '  compiling module-%02d\n' "$i"
  if [ $((i % 3)) -eq 0 ]; then
    printf '  %swarning:%s module-%02d uses a deprecated API\n' "$YELLOW" "$RESET" "$i" >&2
  fi
  sleep 0.3
done
echo

# ---------------------------------------------------------------------------
step "Steady output, one line at a time"
# The section that makes the seam legible. Everything above this point either
# rewrites one row or arrives too fast to watch, so the live window fills up
# and then empties in a blink. Here a line lands roughly every quarter second:
# once the window is full, each new line pushes the oldest one across the seam,
# so you can watch a line stop being rewritable and get a permanent number.
for i in $(seq 1 26); do
  printf '  %s[%02d/26]%s linking %sobject-%02d.o%s\n' \
    "$GREY" "$i" "$RESET" "$CYAN" "$i" "$RESET"
  sleep 0.3
done
echo

# ---------------------------------------------------------------------------
step "A burst, to show batching"
# 400 lines as fast as bash can emit them. These arrive as one or two batched
# chunks rather than 400 network events.
for i in $(seq 1 400); do
  printf '%s  [%03d]%s bundled asset chunk-%03d.js %s(%d KB)%s\n' \
    "$GREY" "$i" "$RESET" "$i" "$DIM" $((RANDOM % 900 + 100)) "$RESET"
done
echo

# ---------------------------------------------------------------------------
step "Test results"
sleep 0.5
printf '  %s✓%s parses simple escape sequences\n' "$GREEN" "$RESET"
sleep 0.3
printf '  %s✓%s carries style across chunk boundaries\n' "$GREEN" "$RESET"
sleep 0.3
printf '  %s✓%s collapses carriage-return rewrites\n' "$GREEN" "$RESET"
sleep 0.3
printf '  %s✗%s resumes after a dropped connection\n' "$RED" "$RESET"
printf '    %sexpected%s 1 stream, %sgot%s 2\n' "$DIM" "$RESET" "$DIM" "$RESET"
echo
printf '  %s3 passed%s, %s1 failed%s %s(that last one is a prop — use "Drop the connection")%s\n' \
  "$GREEN" "$RESET" "$RED" "$RESET" "$DIM" "$RESET"
echo

step "Done"
exit 0
