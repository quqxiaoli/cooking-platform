#!/usr/bin/env bash
# deploy/mysql/init-slave.sh
# Executed by the mysql-slave-init service (a short-lived helper container).
# Both mysql-master and mysql-slave must be healthy before this runs.
#
# Replication setup flow (production-grade, GTID):
# 1. mysqldump master with --set-gtid-purged=ON (consistent snapshot + GTID state)
# 2. Restore dump to slave (slave becomes consistent copy of master at dump point)
# 3. CHANGE REPLICATION SOURCE TO with SOURCE_AUTO_POSITION=1
# 4. START REPLICA (slave asks master for all transactions after what's in dump)
# 5. SET GLOBAL super_read_only=ON (protects slave for container lifetime)
#
# Why mysqldump (not GTID-skip trick):
# Simply setting gtid_purged on an empty slave skips the DDL transactions that
# created the tables. When the SQL thread tries to replay DML from the master
# (e.g. INSERT INTO schema_migrations), the tables don't exist → error 1146.
# mysqldump creates a consistent starting point with all tables + data already
# in place, so replication only needs to apply the delta.
#
# read_only / super_read_only design note:
# NOT in slave.cnf — MySQL Docker entrypoint uses the same config file for its
# temporary init mysqld. super_read_only in conf.d blocks user creation.
# We SET GLOBAL here after the dump restore completes.

set -euo pipefail

MASTER_HOST="${MASTER_HOST:-mysql}"
MASTER_PORT="${MASTER_PORT:-3306}"
SLAVE_HOST="${SLAVE_HOST:-mysql-slave}"
REPL_USER="${REPL_USER:-repl}"
REPL_PASSWORD="${REPL_PASSWORD:-repl123}"
MASTER_ROOT_PASSWORD="${MASTER_ROOT_PASSWORD:-cooking123}"
SLAVE_ROOT_PASSWORD="${SLAVE_ROOT_PASSWORD:-cooking123}"

DUMP_FILE="/tmp/master_snapshot.sql"

echo "[init-slave] Step 1/4: dumping master ${MASTER_HOST}:${MASTER_PORT}..."
mysqldump \
  -h "${MASTER_HOST}" \
  -P "${MASTER_PORT}" \
  -u root \
  -p"${MASTER_ROOT_PASSWORD}" \
  --all-databases \
  --single-transaction \
  --source-data=2 \
  --set-gtid-purged=ON \
  --routines \
  --triggers \
  --events \
  2>/dev/null \
  > "${DUMP_FILE}"
echo "[init-slave] Dump size: $(wc -c < "${DUMP_FILE}") bytes"

echo "[init-slave] Step 2/4: stopping slave & clearing GTID state..."
mysql -h "${SLAVE_HOST}" -u root -p"${SLAVE_ROOT_PASSWORD}" 2>/dev/null <<SQL
STOP REPLICA;
RESET REPLICA ALL;
RESET MASTER;
SQL

echo "[init-slave] Step 3/4: restoring dump to slave..."
mysql -h "${SLAVE_HOST}" -u root -p"${SLAVE_ROOT_PASSWORD}" 2>/dev/null < "${DUMP_FILE}"
rm -f "${DUMP_FILE}"
echo "[init-slave] Dump restored."

echo "[init-slave] Step 4/4: configuring & starting replication..."
mysql -h "${SLAVE_HOST}" -u root -p"${SLAVE_ROOT_PASSWORD}" 2>/dev/null <<SQL
CHANGE REPLICATION SOURCE TO
  SOURCE_HOST='${MASTER_HOST}',
  SOURCE_PORT=${MASTER_PORT},
  SOURCE_USER='${REPL_USER}',
  SOURCE_PASSWORD='${REPL_PASSWORD}',
  SOURCE_AUTO_POSITION=1;
START REPLICA;
SET GLOBAL read_only=ON;
SET GLOBAL super_read_only=ON;
SQL

sleep 2
echo "[init-slave] Replication status:"
mysql -h "${SLAVE_HOST}" -u root -p"${SLAVE_ROOT_PASSWORD}" \
  -e "SHOW REPLICA STATUS\G" 2>/dev/null \
  | grep -E "Replica_(IO|SQL)_Running|Last_IO_Error|Last_SQL_Error|Seconds_Behind"

echo "[init-slave] read_only:"
mysql -h "${SLAVE_HOST}" -u root -p"${SLAVE_ROOT_PASSWORD}" \
  -e "SELECT @@read_only AS read_only, @@super_read_only AS super_read_only;" 2>/dev/null

echo "[init-slave] Done."
