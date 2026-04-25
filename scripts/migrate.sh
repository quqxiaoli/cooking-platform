#!/usr/bin/env bash
# scripts/migrate.sh — convenience wrapper around golang-migrate.
# Usage: ./scripts/migrate.sh up | down | version | force <N>
set -euo pipefail

DSN="${MIGRATE_DSN:-mysql://root:cooking123@tcp(127.0.0.1:3306)/cooking_platform}"
MIGRATIONS="file://$(dirname "$0")/../migrations"
CMD="${1:-up}"

echo "▶  migrate $CMD  (DSN: $DSN)"
migrate -path "$MIGRATIONS" -database "$DSN" "$@"
