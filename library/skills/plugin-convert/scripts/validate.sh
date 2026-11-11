#!/bin/sh
# validate.sh — shape-check a converted plugin source before install.
# Usage: validate.sh <converted-dir>
set -u
out="${1:-}"
if [ -z "$out" ] || [ ! -f "$out/plugin.yaml" ]; then
  echo "validate: plugin.yaml missing" >&2
  exit 1
fi
if ! grep -q '^manifest_version: 2' "$out/plugin.yaml"; then
  echo "validate: manifest_version: 2 missing" >&2
  exit 1
fi
if ! grep -q '^name: ' "$out/plugin.yaml"; then
  echo "validate: manifest name missing" >&2
  exit 1
fi
if [ ! -f "$out/conversion.json" ]; then
  echo "validate: conversion.json missing" >&2
  exit 1
fi
count=$(find "$out/skills" -name SKILL.md 2>/dev/null | wc -l | tr -d ' ')
if [ "$count" -eq 0 ]; then
  echo "validate: no converted skills" >&2
  exit 1
fi
if links=$(find "$out" -type l | head -n 5); [ -n "$links" ]; then
  echo "validate: output contains symlinks (refused):" >&2
  echo "$links" >&2
  exit 1
fi
echo "validate: ok ($count skills)"
