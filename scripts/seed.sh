#!/usr/bin/env bash
# scripts/seed.sh — insert development seed data.
# Will be populated in Step 4 (content module) with sample posts and users.
set -euo pipefail

MYSQL_HOST="${MYSQL_HOST:-127.0.0.1}"
MYSQL_PORT="${MYSQL_PORT:-3306}"
MYSQL_USER="${MYSQL_USER:-root}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-cooking123}"
MYSQL_DATABASE="${MYSQL_DATABASE:-cooking_platform}"

echo "▶  Seeding $MYSQL_DATABASE on $MYSQL_HOST:$MYSQL_PORT"
echo "ℹ️  Seed data will be added here in Step 4."
