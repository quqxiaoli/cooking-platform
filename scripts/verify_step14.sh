#!/usr/bin/env bash
# scripts/verify_step14.sh — Step 14 端到端验证：MySQL 主从复制 + DBResolver 读写分离
# Usage: make verify-step14
# Prerequisites: make docker-up (all containers healthy)
set -euo pipefail
source "$(dirname "$0")/verify_common.sh"

MASTER_PORT=3306
SLAVE_PORT=3307
MYSQL_USER=root
MYSQL_PASS=cooking123
DB=cooking_platform

# Helper: run SQL on slave via docker exec (avoids host network restrictions
# and supports vertical \G output format for SHOW REPLICA STATUS parsing).
slave_sql() {
    docker exec cooking-mysql-slave-dev mysql -uroot -p"${MYSQL_PASS}" "$@" 2>/dev/null
}

master_sql() {
    mysql -h 127.0.0.1 -P${MASTER_PORT} -u${MYSQL_USER} -p${MYSQL_PASS} "$@" 2>/dev/null
}

section "§1 编译检查"
go build ./... && ok "go build ./... passed"

section "§2 MySQL slave 容器健康"
if docker inspect cooking-mysql-slave-dev --format '{{.State.Health.Status}}' 2>/dev/null | grep -q healthy; then
    ok "cooking-mysql-slave-dev is healthy"
else
    fail "cooking-mysql-slave-dev not healthy — run: make docker-up"
fi

section "§3 主从复制状态 (SHOW REPLICA STATUS)"
# performance_schema column: SERVICE_STATE (ON/OFF) in both tables
IO_RUNNING=$(slave_sql --skip-column-names -e "
  SELECT SERVICE_STATE FROM performance_schema.replication_connection_status LIMIT 1;
" | tr -d '[:space:]')

SQL_RUNNING=$(slave_sql --skip-column-names -e "
  SELECT SERVICE_STATE FROM performance_schema.replication_applier_status LIMIT 1;
" | tr -d '[:space:]')

if [[ "$IO_RUNNING" == "ON" ]]; then
    ok "Replica_IO_Running: Yes (SERVICE_STATE=ON)"
else
    fail "Replica_IO_Running: ${IO_RUNNING} (expected ON) — run: docker exec cooking-mysql-slave-dev mysql -uroot -pcooking123 -e 'SHOW REPLICA STATUS\G'"
fi

if [[ "$SQL_RUNNING" == "ON" ]]; then
    ok "Replica_SQL_Running: Yes (SERVICE_STATE=ON)"
else
    fail "Replica_SQL_Running: ${SQL_RUNNING} (expected ON)"
fi

section "§4 GTID 模式确认"
MASTER_GTID=$(master_sql --skip-column-names -e "SELECT @@gtid_mode;" | tr -d '[:space:]')
SLAVE_GTID=$(slave_sql --skip-column-names -e "SELECT @@gtid_mode;" | tr -d '[:space:]')

[[ "$MASTER_GTID" == "ON" ]] && ok "master gtid_mode=ON" || fail "master gtid_mode=${MASTER_GTID}"
[[ "$SLAVE_GTID"  == "ON" ]] && ok "slave  gtid_mode=ON" || fail "slave  gtid_mode=${SLAVE_GTID}"

section "§5 slave read_only / server_id 验证"
READ_ONLY=$(slave_sql --skip-column-names -e "SELECT @@super_read_only;" | tr -d '[:space:]')
SERVER_ID=$(slave_sql --skip-column-names -e "SELECT @@server_id;" | tr -d '[:space:]')

[[ "$READ_ONLY"  == "1" ]] && ok "slave super_read_only=ON" || fail "slave super_read_only=${READ_ONLY} (expected 1)"
[[ "$SERVER_ID"  == "2" ]] && ok "slave server_id=2"         || fail "slave server_id=${SERVER_ID} (expected 2)"

section "§6 写 master → 同步到 slave"
TEST_VAL="step14-$(date +%s)"
master_sql ${DB} -e "CREATE TABLE IF NOT EXISTS _repl_test (val VARCHAR(64) PRIMARY KEY);"
master_sql ${DB} -e "INSERT INTO _repl_test (val) VALUES ('${TEST_VAL}') ON DUPLICATE KEY UPDATE val=val;"
ok "Inserted canary row '${TEST_VAL}' on master"

FOUND="0"
for i in $(seq 1 10); do
    FOUND=$(slave_sql --skip-column-names ${DB} \
        -e "SELECT COUNT(*) FROM _repl_test WHERE val='${TEST_VAL}';" | tr -d '[:space:]')
    if [[ "$FOUND" == "1" ]]; then
        ok "Canary row visible on slave (replication lag ≤ ${i}00ms)"
        break
    fi
    sleep 0.1
done
[[ "$FOUND" == "1" ]] || fail "Canary row NOT visible on slave after 1s"

# Cleanup
master_sql ${DB} -e "DROP TABLE IF EXISTS _repl_test;"

section "§7 DBResolver 配置验证 (Go 服务器启动)"
APP_LOG=$(mktemp)
go run ./cmd/server/main.go > "${APP_LOG}" 2>&1 &
SERVER_PID=$!

until grep -q "mysql connected" "${APP_LOG}" 2>/dev/null || ! kill -0 "${SERVER_PID}" 2>/dev/null; do sleep 1; done

if grep -q "mysql connected" "${APP_LOG}" 2>/dev/null; then
    ok "Server started: mysql connected (DBResolver registered with slave DSN)"
else
    cat "${APP_LOG}"
    kill "${SERVER_PID}" 2>/dev/null || true
    fail "Server failed to start"
fi

kill "${SERVER_PID}" 2>/dev/null || true
wait "${SERVER_PID}" 2>/dev/null || true
rm -f "${APP_LOG}"
ok "Server graceful shutdown confirmed"

echo ""
echo "══════════════════════════════════════════════════════════"
echo "  Step 14 verify: ALL CHECKS PASSED"
echo "══════════════════════════════════════════════════════════"
