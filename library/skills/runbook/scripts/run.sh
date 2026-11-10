#!/bin/sh
# Example helper: list the steps of a runbook plan.
set -u
grep -E '^[0-9]+\.' "${1:-/dev/stdin}"
