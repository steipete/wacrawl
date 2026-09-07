#!/usr/bin/env bash
set -euo pipefail

export GOWORK=off
sqlite_mod="$(go list -m -f '{{.GoMod}}' modernc.org/sqlite)"
expected="$(awk '$1 == "modernc.org/libc" { print $2 } $1 == "require" && $2 == "modernc.org/libc" { print $3 }' "$sqlite_mod")"
actual="$(go list -m -f '{{.Version}}' modernc.org/libc)"

# SQLite's generated code requires its exact libc runtime, even for patch updates.
if [[ -z "$expected" || "$actual" != "$expected" ]]; then
  printf 'SQLite runtime mismatch: selected libc %s; SQLite requires %s. Update SQLite and its runtime together.\n' "$actual" "${expected:-unknown}" >&2
  exit 1
fi
printf 'SQLite runtime matches: modernc.org/libc %s\n' "$actual"
