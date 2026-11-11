#!/bin/sh
# stage.sh — read-only source hygiene check. Prints a summary or fails closed.
# Usage: stage.sh <source-dir>
set -u
src="${1:-}"
if [ -z "$src" ] || [ ! -d "$src" ]; then
  echo "stage: source directory is unavailable" >&2
  exit 1
fi
if links=$(find "$src" -type l | head -n 5); [ -n "$links" ]; then
  echo "stage: source contains symlinks (refused):" >&2
  echo "$links" >&2
  exit 1
fi
if big=$(find "$src" -type f -size +1048576c | head -n 5); [ -n "$big" ]; then
  echo "stage: source contains files over 1 MiB (refused):" >&2
  echo "$big" >&2
  exit 1
fi
count=$(find "$src" -type f | wc -l | tr -d ' ')
echo "stage: ok ($count files, no symlinks, nothing over 1 MiB)"
