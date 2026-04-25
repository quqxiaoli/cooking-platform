#!/usr/bin/env bash
# scripts/backup.sh — mysqldump to timestamped file.
# Usage: ./scripts/backup.sh [output_dir]
set -euo pipefail

OUTPUT_DIR="${1:-./backups}"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
FILENAME="$OUTPUT_DIR/cooking_platform_${TIMESTAMP}.sql.gz"
mkdir -p "$OUTPUT_DIR"

MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
MYSQL_USER="${MYSQL_USER:-root}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-cooking123}"

echo "▶  Backing up cooking_platform → $FILENAME"
mysqldump \
  -h "$MYSQL_HOST" -P "$MYSQL_PORT" \
  -u "$MYSQL_USER" -p"$MYSQL_PASSWORD" \
  --single-transaction --routines --triggers \
  cooking_platform | gzip > "$FILENAME"

echo "✅  Backup complete: $FILENAME ($(du -sh "$FILENAME" | cut -f1))"
