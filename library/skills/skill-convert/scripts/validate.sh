#!/bin/sh
# validate.sh — shape-check a converted skill source before install.
# Usage: validate.sh <converted-dir>
set -u
out="${1:-}"
count=$(find "$out" -name SKILL.md 2>/dev/null | wc -l | tr -d ' ')
if [ "$count" -eq 0 ]; then
  echo "validate: no converted skills" >&2
  exit 1
fi
if [ ! -f "$out/conversion.json" ]; then
  echo "validate: conversion.json missing" >&2
  exit 1
fi
if links=$(find "$out" -type l | head -n 5); [ -n "$links" ]; then
  echo "validate: output contains symlinks (refused):" >&2
  echo "$links" >&2
  exit 1
fi
echo "validate: ok ($count skills)"
