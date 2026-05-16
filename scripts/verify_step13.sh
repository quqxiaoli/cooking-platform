#!/usr/bin/env bash
# verify_step13.sh — Step 13: EventBus 切换 RabbitMQ（生产加固验证）
#
# 验证范围：
#   §1  编译检查
#   §2  RabbitMQ 容器健康
#   §3  启动 server（rabbitmq provider）
#   §4  Exchange 拓扑（cooking.events + cooking.events.dlx）
#   §5  DLX queue（cooking.events.dlx.queue）
#   §6  at-least-once 集成验证：like → MySQL confirm → unlike → MySQL confirm
#   §7  命名持久化队列（cooking.event.like）
#
# 前置条件：
#   make docker-up   （MySQL + Redis + RabbitMQ 均需运行）
#   RabbitMQ: cooking/cooking123 @ 127.0.0.1:5672
set -euo pipefail
source "$(dirname "$0")/verify_common.sh"

RABBITMQ_CONTAINER="${RABBITMQ_CONTAINER:-cooking-rabbitmq-dev}"
RABBITMQ_URL="${RABBITMQ_URL:-amqp://cooking:cooking123@127.0.0.1:5672/}"
SERVER_LOG="logs/step13_verify.log"
SERVER_PID=""

cleanup() {
  # Kill the go run wrapper if still alive.
  if [ -n "${SERVER_PID}" ] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill "${SERVER_PID}" 2>/dev/null
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  # go run spawns the actual server binary as a child; kill by port.
  local port_pid
  port_pid=$(lsof -ti :8080 2>/dev/null || true)
  if [ -n "${port_pid}" ]; then
    kill "${port_pid}" 2>/dev/null || true
    sleep 1
  fi
}
trap cleanup EXIT

# ── §1 编译检查 ────────────────────────────────────────────────────────────
section "§1 编译检查"
go build ./... && ok "go build ./... passed"

# ── §2 RabbitMQ 容器健康 ──────────────────────────────────────────────────
section "§2 RabbitMQ 容器健康检查"
docker ps --format '{{.Names}}' | grep -q "^${MYSQL_CONTAINER}$" \
  || fail "MySQL 容器 ${MYSQL_CONTAINER} 未运行 —— 请先 make docker-up"
docker ps --format '{{.Names}}' | grep -q "^${REDIS_CONTAINER}$" \
  || fail "Redis 容器 ${REDIS_CONTAINER} 未运行 —— 请先 make docker-up"
docker ps --format '{{.Names}}' | grep -q "^${RABBITMQ_CONTAINER}$" \
  || fail "RabbitMQ 容器 ${RABBITMQ_CONTAINER} 未运行 —— 请先 make docker-up"
docker exec "${RABBITMQ_CONTAINER}" rabbitmq-diagnostics ping -q \
  && ok "RabbitMQ ping OK"

# ── §3 启动 server（rabbitmq provider）────────────────────────────────────
section "§3 启动 server（rabbitmq provider）"
# 清理可能残留的旧 server（避免端口冲突）
OLD_PID=$(lsof -ti :8080 2>/dev/null || true)
if [ -n "${OLD_PID}" ]; then
  info "发现旧 server (PID=${OLD_PID})，先停掉..."
  kill "${OLD_PID}" 2>/dev/null || true
  sleep 2
fi
mkdir -p logs
APP_MQ_PROVIDER=rabbitmq APP_MQ_URL="${RABBITMQ_URL}" \
  go run ./cmd/server >"${SERVER_LOG}" 2>&1 &
SERVER_PID=$!
info "server PID=${SERVER_PID}，等待就绪（最多 40s）..."

for i in $(seq 1 40); do
  if curl -sf http://localhost:8080/health >/dev/null 2>&1; then
    ok "server started (PID=${SERVER_PID})"
    break
  fi
  sleep 1
  [ $i -eq 40 ] && fail "server 启动超时（40s），检查 ${SERVER_LOG}"
done

# ── §4 Exchange 拓扑验证 ──────────────────────────────────────────────────
# 注意：rabbitmqctl output | grep -q 在 pipefail 下会因 grep 提前退出导致
# rabbitmqctl 收到 SIGPIPE（exit 141），进而 pipefail 判定整体失败。
# 解决方案：先将输出写入临时文件，再 grep。
section "§4 Exchange 拓扑验证"
RMQ_EXCHANGES=$(mktemp)
docker exec "${RABBITMQ_CONTAINER}" rabbitmqctl list_exchanges >"${RMQ_EXCHANGES}" 2>&1
grep -q "cooking.events" "${RMQ_EXCHANGES}" \
  && ok "cooking.events exchange 存在" \
  || fail "cooking.events exchange 未声明"
grep -q "cooking.events.dlx" "${RMQ_EXCHANGES}" \
  && ok "cooking.events.dlx exchange 存在" \
  || fail "cooking.events.dlx exchange 未声明"
rm -f "${RMQ_EXCHANGES}"

# ── §5 DLX queue 验证 ─────────────────────────────────────────────────────
section "§5 DLX Queue 验证"
RMQ_QUEUES=$(mktemp)
docker exec "${RABBITMQ_CONTAINER}" rabbitmqctl list_queues >"${RMQ_QUEUES}" 2>&1
grep -q "cooking.events.dlx.queue" "${RMQ_QUEUES}" \
  && ok "cooking.events.dlx.queue 存在" \
  || fail "cooking.events.dlx.queue 未声明"
rm -f "${RMQ_QUEUES}"

# ── §6 at-least-once 集成验证 ────────────────────────────────────────────
section "§6 at-least-once 集成验证（like → MySQL confirm → unlike → MySQL confirm）"
LOG_FILE="${SERVER_LOG}"

info "注册/登录用户..."
LOGIN_RESP=$(login_resp 13988138013)
TOKEN=$(echo "${LOGIN_RESP}" | jq -r '.data.access_token')
USER_ID=$(echo "${LOGIN_RESP}" | jq -r '.data.user.id')
assert_nonempty "${TOKEN}"   "获取 access_token 成功"
assert_nonempty "${USER_ID}" "获取 user_id 成功"

info "发布帖子..."
POST_RESP=$(curl -s -X POST "${BASE}/posts" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"title":"Step13 RabbitMQ 验证帖","content":"at-least-once verify","scene_tag":1}')
assert_code "0" "${POST_RESP}" "发布帖子成功"
POST_ID=$(echo "${POST_RESP}" | jq -r '.data.post_id')
assert_nonempty "${POST_ID}" "获取 post_id 成功"

info "点赞帖子（触发 TopicLike → LikeConsumer 通过 RabbitMQ）..."
LIKE_RESP=$(curl -s -X POST "${BASE}/posts/${POST_ID}/like" \
  -H "Authorization: Bearer ${TOKEN}")
assert_code "0" "${LIKE_RESP}" "点赞请求返回 code=0"

info "等待 LikeConsumer flush（flush_interval=3s + buffer 2s）..."
sleep 5

info "验证 likes 表写入..."
LIKE_CNT=$(mysql_q "SELECT COUNT(*) FROM likes WHERE user_id=${USER_ID} AND post_id=${POST_ID}")
assert_eq "1" "${LIKE_CNT}" "likes 表有对应记录（事件通过 RabbitMQ 消费并落库）"

info "验证 posts.like_count 更新..."
LIKE_COUNT=$(mysql_q "SELECT like_count FROM posts WHERE id=${POST_ID}")
assert_eq "1" "${LIKE_COUNT}" "posts.like_count=1"

info "取消点赞（触发 TopicUnlike）..."
UNLIKE_RESP=$(curl -s -X DELETE "${BASE}/posts/${POST_ID}/like" \
  -H "Authorization: Bearer ${TOKEN}")
assert_code "0" "${UNLIKE_RESP}" "取消点赞请求返回 code=0"

sleep 5

info "验证 likes 表行已删除..."
LIKE_CNT_AFTER=$(mysql_q "SELECT COUNT(*) FROM likes WHERE user_id=${USER_ID} AND post_id=${POST_ID}")
assert_eq "0" "${LIKE_CNT_AFTER}" "likes 表记录已清除（unlike 事件通过 RabbitMQ 消费落库）"

info "验证 posts.like_count 回归 0..."
LIKE_COUNT_AFTER=$(mysql_q "SELECT like_count FROM posts WHERE id=${POST_ID}")
assert_eq "0" "${LIKE_COUNT_AFTER}" "posts.like_count=0"

# ── §7 命名持久化队列验证 ────────────────────────────────────────────────
section "§7 命名持久化队列验证（cooking.event.like）"
RMQ_QUEUES2=$(mktemp)
docker exec "${RABBITMQ_CONTAINER}" rabbitmqctl list_queues >"${RMQ_QUEUES2}" 2>&1
grep -q "cooking.event.like" "${RMQ_QUEUES2}" \
  && ok "cooking.event.like 队列存在（LikeConsumer 已声明，durable=true）" \
  || fail "cooking.event.like 队列未找到 —— LikeConsumer 可能未成功 Subscribe"
rm -f "${RMQ_QUEUES2}"

banner_pass "Step 13 EventBus RabbitMQ 生产加固"
