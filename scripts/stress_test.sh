#!/usr/bin/env bash
# scripts/stress_test.sh — Step 20 阶段 4 压测
#
# 设计为 prod 公网场景，跑在 cooking-platform 服务器本机。
#
# 5 个 section：
#   §1  前置检查（wrk/curl/jq 可用、prod 容器全 healthy）
#   §2  Setup：注册用户 1（发帖人）+ 用户 2（点赞人），发一篇帖子，
#       把 POST_ID + TOKEN_2 写入 /tmp/stress_ctx_step20 供 lua 读取
#   §3  wrk 压 5 个场景（/health, feed, detail, search, like）
#   §4  Consumer 异步落库延迟实证（posts.like_count 3 点采样）
#   §5  Grafana 截图提醒 + 汇总输出
#
# 用法：
#   source .env.prod && bash scripts/stress_test.sh
#   # 或：MYSQL_ROOT_PW=xxx bash scripts/stress_test.sh

set -u

# ── 公共常量 ────────────────────────────────────────────────────────────────
BASE="${BASE:-https://mellowck.com/api/v1}"
HOST="${HOST:-https://mellowck.com}"
APP1_CONTAINER="${APP1_CONTAINER:-cooking-app1-prod}"
APP2_CONTAINER="${APP2_CONTAINER:-cooking-app2-prod}"
NGINX_CONTAINER="${NGINX_CONTAINER:-cooking-nginx-prod}"
MYSQL_MASTER_CONTAINER="${MYSQL_MASTER_CONTAINER:-cooking-mysql-prod}"
MYSQL_SLAVE_CONTAINER="${MYSQL_SLAVE_CONTAINER:-cooking-mysql-slave-prod}"
REDIS_CONTAINER="${REDIS_CONTAINER:-cooking-redis-prod}"
MYSQL_ROOT_PW="${MYSQL_ROOT_PW:-${MYSQL_ROOT_PASSWORD:-}}"

CTX_FILE="/tmp/stress_ctx_step20"
RESULT_DIR="/tmp/stress_results_step20"

WRK_THREADS="${WRK_THREADS:-1}"
WRK_CONNS="${WRK_CONNS:-5}"           # 单 IP 自压测必须 ≤ nginx 100r/s + burst 20
WRK_DURATION="${WRK_DURATION:-30s}"

C_G='\033[0;32m'; C_R='\033[0;31m'; C_Y='\033[1;33m'; C_N='\033[0m'

PASS_COUNT=0
FAIL_COUNT=0
WARN_COUNT=0

ok()   { echo -e "${C_G}✓${C_N} $1"; PASS_COUNT=$((PASS_COUNT+1)); }
bad()  { echo -e "${C_R}✗${C_N} $1"; FAIL_COUNT=$((FAIL_COUNT+1)); }
warn() { echo -e "${C_Y}⚠${C_N} $1"; WARN_COUNT=$((WARN_COUNT+1)); }
info() { echo -e "${C_Y}→${C_N} $1"; }
section() { echo ""; echo -e "${C_Y}── $1 ──${C_N}"; }

# ── 容器内 SQL helper（同 verify_step20.sh） ───────────────────────────────
mysql_q() {
  local container="$1" sql="$2"
  docker exec "$container" mysql -uroot -p"$MYSQL_ROOT_PW" -N -B -e "$sql" 2>/dev/null
}

# 抓某手机号最新一条 [SMS MOCK] 验证码（与 verify_step20.sh 一致）
fetch_sms_code() {
  local phone="$1"
  local line
  line=$(
    { docker exec "$APP1_CONTAINER" sh -c "grep -F '\"phone\":\"$phone\"' /var/log/cooking/app.log" 2>/dev/null;
      docker exec "$APP2_CONTAINER" sh -c "grep -F '\"phone\":\"$phone\"' /var/log/cooking/app.log" 2>/dev/null; } \
      | grep -F '[SMS MOCK] verification code sent' \
      | tail -1
  )
  if [ -z "$line" ]; then
    echo ""
    return
  fi
  echo "$line" | jq -r '.code'
}

# ════════════════════════════════════════════════════════════════════════════
# §1 前置检查
# ════════════════════════════════════════════════════════════════════════════
section "§1 前置检查"

command -v wrk  >/dev/null && ok "wrk 已安装"  || { bad "需要 wrk (apt-get install -y wrk)"; exit 1; }
command -v jq   >/dev/null && ok "jq 已安装"   || { bad "需要 jq";   exit 1; }
command -v curl >/dev/null && ok "curl 已安装" || { bad "需要 curl"; exit 1; }

if [ -z "$MYSQL_ROOT_PW" ]; then
  bad "MYSQL_ROOT_PW 未设置 —— 请先 'source .env.prod' 或显式传入 (MYSQL_ROOT_PW=... bash $0)"
  exit 1
else
  ok "MYSQL_ROOT_PW 已注入（长度 ${#MYSQL_ROOT_PW}）"
fi

for cname in "$APP1_CONTAINER" "$APP2_CONTAINER" "$NGINX_CONTAINER" \
             "$MYSQL_MASTER_CONTAINER" "$MYSQL_SLAVE_CONTAINER"; do
  if docker ps --format '{{.Names}}' | grep -q "^${cname}$"; then
    ok "$cname running"
  else
    bad "$cname 未运行"
  fi
done

if [ "$FAIL_COUNT" -gt 0 ]; then
  bad "前置检查失败，终止压测"
  exit 1
fi

mkdir -p "$RESULT_DIR"
info "压测结果目录: $RESULT_DIR"

# ════════════════════════════════════════════════════════════════════════════
# §2 Setup：注册用户 + 发帖 + 写 stress_ctx_step20
# ════════════════════════════════════════════════════════════════════════════
section "§2 Setup（注册用户 1 / 2，发帖，写入压测上下文）"

# —— 用户 1（发帖人） ——
PHONE1="138$(printf '%08d' $((RANDOM*RANDOM % 100000000)))"
info "  用户 1 手机号: $PHONE1"

curl -sS -X POST "$BASE/auth/send-code" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE1\"}" >/dev/null
sleep 0.5

CODE1=$(fetch_sms_code "$PHONE1")
if [ -z "$CODE1" ]; then bad "未抓到用户 1 验证码"; exit 1; fi
ok "抓到用户 1 验证码: $CODE1"

LOGIN1=$(curl -sS -X POST "$BASE/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE1\",\"code\":\"$CODE1\"}")
TOKEN1=$(echo "$LOGIN1" | jq -r '.data.access_token // empty')
if [ -z "$TOKEN1" ]; then bad "用户 1 登录失败: $LOGIN1"; exit 1; fi
ok "用户 1 登录拿到 token"

# 发帖（标题/正文含 "verify" 关键字，让 §3 的 wrk_get_search.lua 能命中）
MARK=$(date +%s)_$RANDOM
TITLE="stress verify $MARK"
CONTENT="stress_test.sh setup verify keyword post for mark=${MARK}"

POST_BODY=$(jq -nc \
  --arg title "$TITLE" \
  --arg content "$CONTENT" \
  '{ title: $title, scene_tag: 1, content: $content }')

POST_RESP=$(curl -sS -X POST "$BASE/posts" \
  -H "Authorization: Bearer $TOKEN1" \
  -H 'Content-Type: application/json' \
  -d "$POST_BODY")
POST_ID=$(echo "$POST_RESP" | jq -r '.data.post_id // empty')
if [ -z "$POST_ID" ]; then bad "发帖失败: $POST_RESP"; exit 1; fi
ok "发帖成功 post_id=$POST_ID"

# —— 用户 2（点赞人） ——
# prod-only 临时回避：单机自压测两次 send-code 共用同一出口 IP，会被应用层
# SMS per-IP 限流挡住。真实生产场景用户来自不同 IP，不会有此问题。
info "等待 8 秒让 SMS 限流自然衰减（防止两次 send-code 间隔过短）"
sleep 8

PHONE2="138$(printf '%08d' $(((RANDOM*RANDOM+1) % 100000000)))"
info "  用户 2 手机号: $PHONE2"

curl -sS -X POST "$BASE/auth/send-code" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE2\"}" >/dev/null
sleep 0.5

CODE2=$(fetch_sms_code "$PHONE2")
if [ -z "$CODE2" ]; then bad "未抓到用户 2 验证码"; exit 1; fi
ok "抓到用户 2 验证码: $CODE2"

LOGIN2=$(curl -sS -X POST "$BASE/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE2\",\"code\":\"$CODE2\"}")
TOKEN2=$(echo "$LOGIN2" | jq -r '.data.access_token // empty')
if [ -z "$TOKEN2" ]; then bad "用户 2 登录失败: $LOGIN2"; exit 1; fi
ok "用户 2 登录拿到 token"

# 写入上下文文件：第一行 POST_ID，第二行 TOKEN_2
printf '%s\n%s\n' "$POST_ID" "$TOKEN2" > "$CTX_FILE"
chmod 644 "$CTX_FILE"
ok "压测上下文写入 $CTX_FILE"

# ════════════════════════════════════════════════════════════════════════════
# §3 wrk 压测 5 个场景
# ════════════════════════════════════════════════════════════════════════════
section "§3 wrk 压测（${WRK_THREADS}t / ${WRK_CONNS}c / ${WRK_DURATION}）"

echo ""
echo -e "${C_Y}注：单机自压测受 nginx per-IP 100r/s 限流，无法测极限 QPS。${C_N}"
echo -e "${C_Y}本步用 ${WRK_THREADS}t/${WRK_CONNS}c 测「合理负载下的延迟分布」P50/P90/P99。${C_N}"
echo -e "${C_Y}极限 QPS 需多节点客户端（k6 cloud / 分布式 wrk）才能测出。${C_N}"

run_wrk() {
  local scene="$1"
  local out="$RESULT_DIR/${scene}.txt"
  shift
  info "→ 场景 [$scene]"
  if wrk --latency -t"$WRK_THREADS" -c"$WRK_CONNS" -d"$WRK_DURATION" "$@" 2>&1 | tee "$out" >/dev/null; then
    ok "[$scene] 压测完成 → $out"
  else
    bad "[$scene] 压测失败"
  fi
}

# 压测前先采一次 like_count（§4 三点采样的第 1 点）
LIKE_BEFORE=$(mysql_q "$MYSQL_MASTER_CONTAINER" \
  "SELECT like_count FROM cooking_platform.posts WHERE id=$POST_ID")
info "  like_count BEFORE 压测 = ${LIKE_BEFORE:-0}"

# a. /health（nginx 直返，不打到 backend）
run_wrk "health" "$HOST/health"

# b. GET /feed
run_wrk "feed" -s scripts/stress/wrk_get_feed.lua "$HOST"

# c. GET /posts/:id
run_wrk "detail" -s scripts/stress/wrk_get_detail.lua "$HOST"

# d. GET /search
run_wrk "search" -s scripts/stress/wrk_get_search.lua "$HOST"

# e. POST /posts/:id/like
run_wrk "like" -s scripts/stress/wrk_post_like.lua "$HOST"

# ════════════════════════════════════════════════════════════════════════════
# §4 Consumer 异步落库延迟实证
# ════════════════════════════════════════════════════════════════════════════
section "§4 Consumer 异步落库延迟（posts.like_count 3 点采样）"

# 第 2 点：压测刚结束（数据可能还在 Redis，未 drain）
LIKE_IMMEDIATE=$(mysql_q "$MYSQL_MASTER_CONTAINER" \
  "SELECT like_count FROM cooking_platform.posts WHERE id=$POST_ID")
info "  like_count IMMEDIATE 压测后 = ${LIKE_IMMEDIATE:-0}"

# 等 15 秒让 LikeConsumer 把 Redis 累积值 drain 进 MySQL
info "  等 15 秒让 LikeConsumer 完成 drain ..."
sleep 15

# 第 3 点：Consumer 已 drain
LIKE_AFTER=$(mysql_q "$MYSQL_MASTER_CONTAINER" \
  "SELECT like_count FROM cooking_platform.posts WHERE id=$POST_ID")
info "  like_count AFTER drain（+15s） = ${LIKE_AFTER:-0}"

# 从 like 场景的 wrk 结果里捞「成功请求数」作为压测期间应当累积的点赞数
# 注：同一用户对同一 post 点赞幂等，DB 最终只会 +1，所以这里只是看
#     「IMMEDIATE → AFTER 是否还在涨」来证明异步通道存在。
LIKE_REQ_TOTAL=$(grep -E 'requests in' "$RESULT_DIR/like.txt" 2>/dev/null | awk '{print $1}' || echo "0")
info "  like 场景压测请求总数 ≈ ${LIKE_REQ_TOTAL:-0}"

DELTA_FULL=$(( ${LIKE_AFTER:-0} - ${LIKE_BEFORE:-0} ))
DELTA_LAG=$((  ${LIKE_AFTER:-0} - ${LIKE_IMMEDIATE:-0} ))
info "  BEFORE → AFTER 总增量      = $DELTA_FULL"
info "  IMMEDIATE → AFTER（异步窗）= $DELTA_LAG"

if [ "$DELTA_FULL" -gt 0 ]; then
  ok "压测期间点赞数据成功落库（+$DELTA_FULL）"
else
  warn "未观察到点赞落库 —— 检查 LikeConsumer / 业务可能因 pending 拒绝交互"
fi

if [ "$DELTA_LAG" -gt 0 ]; then
  ok "观察到异步通道（IMMEDIATE 之后 like_count 仍上涨 $DELTA_LAG）—— LikeConsumer 写库存在异步窗"
else
  info "IMMEDIATE → AFTER 无增量 —— Consumer 在压测期内已 drain 完，或本机点赞被去重"
fi

# ════════════════════════════════════════════════════════════════════════════
# §5 Grafana 提醒 + 汇总
# ════════════════════════════════════════════════════════════════════════════
section "§5 Grafana 截图提醒"

cat <<'EOF'
请在浏览器访问 http://47.238.29.251:3000，截图以下 4 块看板归档:
  - HTTP Request Latency (P50/P95/P99)
  - Consumer Backlog (LikeConsumer / PVConsumer / AuditConsumer)
  - MySQL Connection Pool Usage
  - Redis Command Latency

Grafana 截图保存到 docs/stress/step20_<scene>.png
EOF
echo ""
echo "压测结果已保存到 $RESULT_DIR/"

# —— 汇总表 ——
section "汇总（QPS / P50 / P95 / P99 / 错误率）"

summarize() {
  local scene="$1" out="$RESULT_DIR/${scene}.txt"
  if [ ! -f "$out" ]; then
    printf "  %-8s  (no result file)\n" "$scene"
    return
  fi
  local qps p50 p90 p99 nonok total errs
  qps=$(grep -E '^Requests/sec:' "$out"   | awk '{print $2}')
  p50=$(grep -E '^\s+50%' "$out"          | awk '{print $2}')
  p90=$(grep -E '^\s+90%' "$out"          | awk '{print $2}')
  p99=$(grep -E '^\s+99%' "$out"          | awk '{print $2}')
  # wrk 错误：Non-2xx or 3xx responses / Socket errors
  nonok=$(grep -E 'Non-2xx or 3xx responses' "$out" | awk '{print $NF}')
  total=$(grep -E 'requests in' "$out" | awk '{print $1}')
  errs=$(grep -E '^\s+Socket errors' "$out" | sed -E 's/.*connect ([0-9]+).*read ([0-9]+).*write ([0-9]+).*timeout ([0-9]+).*/c=\1 r=\2 w=\3 t=\4/' || echo "-")
  printf "  %-8s  QPS=%-10s  P50=%-8s  P90=%-8s  P99=%-8s  non2xx=%-6s total=%-7s  sockets=%s\n" \
    "$scene" "${qps:--}" "${p50:--}" "${p90:--}" "${p99:--}" "${nonok:-0}" "${total:--}" "${errs:--}"
}

printf "  %-8s  %-14s  %-12s  %-12s  %-12s  %-12s %-12s %s\n" \
  scene QPS P50 P90 P99 non2xx total sock_errors
echo  "  -----------------------------------------------------------------------------------------------"
for scene in health feed detail search like; do
  summarize "$scene"
done

# ════════════════════════════════════════════════════════════════════════════
# 退出
# ════════════════════════════════════════════════════════════════════════════
echo ""
echo -e "${C_Y}──────────────────────────────────────────${C_N}"
echo -e "${C_G}✓ ${PASS_COUNT} passed${C_N}"
echo -e "${C_R}✗ ${FAIL_COUNT} failed${C_N}"
echo -e "${C_Y}⚠ ${WARN_COUNT} warnings${C_N}"
echo -e "${C_Y}──────────────────────────────────────────${C_N}"

if [ "$FAIL_COUNT" -gt 0 ]; then
  exit 1
fi
exit 0
