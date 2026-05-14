#!/usr/bin/env bash
# scripts/verify_common.sh
#
# 端到端验证脚本的公共库 —— 环境检查、登录取 token、docker 断言、断言函数库。
#
# ── 用法 ───────────────────────────────────────────────────────────────────
#
# 每个 verify_step_N.sh (N>=8) 在开头 source 本文件,然后直接调用其中的
# 函数,不要重写已有的通用片段：
#
#   #!/usr/bin/env bash
#   set -e
#   source "$(dirname "$0")/verify_common.sh"
#
#   precheck                       # 检查 logs/dev.log + jq
#   TOKEN=$(token_of 13800138000)  # 一行拿到 access_token
#   RESP=$(curl -s "$BASE/whatever")
#   assert_code 0 "$RESP" "whatever 应成功"
#
# ── 边界说明 ───────────────────────────────────────────────────────────────
#
#   - 本库 NOT 设 `set -e`：一个被 source 的库不应改变调用方的执行模式。
#     `set -e` 由各 verify_step_N.sh 自己在开头声明。
#   - verify_step5/6/7.sh 是在本库存在之前写的,保持自包含、不回头重构 ——
#     它们已验证通过,改动它们没有收益只有回归风险。本库只服务 Step 8+。
#   - 公共常量 (BASE / LOG_FILE / 容器名 / DB 凭据) 用 `${VAR:-default}` 暴露,
#     调用方可在 source 之前用环境变量覆盖,例如 BASE=... bash verify_step8.sh。
#
# ── macOS 兼容性备忘 (Step 6 踩坑沉淀) ─────────────────────────────────────
#
#   - bash 3.2 不支持数组负索引 [-1],用 ${arr[$((${#arr[@]}-1))]}
#   - 不支持 `head -n -1`,用 `sed '$d'`
#   - 起 server 用 `tee -i`,防 Ctrl+C 的 SIGPIPE 吞掉 shutdown 日志
#   - 抓 SMS code 别用 `grep -oE '[0-9]{6}'` —— 会命中手机号；用下方 token_of 的精确匹配
#
# Added before Step 8 (follow module) to retire the long-described but
# never-created shared-script abstraction.

# ── 公共常量 (可被环境变量覆盖) ─────────────────────────────────────────────
BASE="${BASE:-http://localhost:8080/api/v1}"
LOG_FILE="${LOG_FILE:-logs/dev.log}"
MYSQL_CONTAINER="${MYSQL_CONTAINER:-cooking-mysql-dev}"
REDIS_CONTAINER="${REDIS_CONTAINER:-cooking-redis-dev}"
MYSQL_DB="${MYSQL_DB:-cooking_platform}"
MYSQL_ROOT_PW="${MYSQL_ROOT_PW:-cooking123}"

# ── 颜色与日志 (与 verify_step5/7.sh 视觉一致) ──────────────────────────────
C_G='\033[0;32m'; C_R='\033[0;31m'; C_Y='\033[1;33m'; C_N='\033[0m'

# ok / fail / info —— 三个基础输出函数。fail 会 exit 1 终止脚本。
ok()   { echo -e "${C_G}✓${C_N} $1"; }
fail() { echo -e "${C_R}✗${C_N} $1"; exit 1; }
info() { echo -e "${C_Y}→${C_N} $1"; }

# section —— 打印一个带分隔线的小节标题,让长脚本输出更易读。
section() {
  echo ""
  echo -e "${C_Y}── $1 ${C_N}"
}

# banner_pass —— 脚本结尾的全绿横幅。用法: banner_pass "Step 8 关注模块"
banner_pass() {
  echo ""
  echo -e "${C_G}========================================${C_N}"
  echo -e "${C_G}  $1 端到端验证全部通过 ✓${C_N}"
  echo -e "${C_G}========================================${C_N}"
  echo ""
}

# ── 前置检查 ────────────────────────────────────────────────────────────────

# precheck —— 验证脚本运行的前置条件。任一不满足就 fail 退出。
#   1. logs/dev.log 存在 (server 须用 `... 2>&1 | tee -i logs/dev.log` 启动)
#   2. jq 已安装 (所有断言依赖 jq 解析 JSON)
#   3. docker 开发栈容器在跑
precheck() {
  [ -f "$LOG_FILE" ] || fail "日志文件 $LOG_FILE 不存在 —— 请用 'go run ./cmd/server 2>&1 | tee -i logs/dev.log' 启动服务"
  command -v jq >/dev/null || fail "需要 jq 命令 (brew install jq)"
  docker ps --format '{{.Names}}' | grep -q "^${MYSQL_CONTAINER}$" \
    || fail "MySQL 容器 ${MYSQL_CONTAINER} 未运行 —— 请先 make docker-up"
  docker ps --format '{{.Names}}' | grep -q "^${REDIS_CONTAINER}$" \
    || fail "Redis 容器 ${REDIS_CONTAINER} 未运行 —— 请先 make docker-up"
}

# ── 登录辅助 ────────────────────────────────────────────────────────────────

# login_resp —— 给定手机号,完成 send-code → 抓验证码 → login,返回完整 login JSON。
# 用法: RESP=$(login_resp 13800138000)
#
# 抓码用精确匹配 '"code":[[:space:]]*"[0-9]+"',避免命中日志里的手机号
# (Step 6 踩坑：宽松的 [0-9]{6} 会误匹配手机号前缀)。
login_resp() {
  local phone="$1"
  curl -s -X POST "$BASE/auth/send-code" \
    -H 'Content-Type: application/json' \
    -d "{\"phone\":\"$phone\"}" >/dev/null
  sleep 0.3  # 等日志落盘
  local code
  code=$(grep '\[SMS MOCK\]' "$LOG_FILE" | tail -1 \
    | grep -oE '"code":[[:space:]]*"[0-9]+"' | grep -oE '[0-9]+')
  [ -n "$code" ] || fail "日志中找不到验证码 (grep '[SMS MOCK]' $LOG_FILE)"
  curl -s -X POST "$BASE/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"phone\":\"$phone\",\"code\":\"$code\"}"
}

# token_of —— 给定手机号,直接返回 access_token (最常用,封装掉 login JSON 解析)。
# 用法: TOKEN=$(token_of 13800138000)
token_of() {
  local resp token
  resp=$(login_resp "$1")
  token=$(echo "$resp" | jq -r '.data.access_token')
  [ "$token" != "null" ] && [ -n "$token" ] || fail "登录失败,拿不到 token: $resp"
  echo "$token"
}

# uid_of —— 给定手机号,返回 user_id。用法: UID=$(uid_of 13800138000)
# 注意：会触发一次完整 login。若同时要 token 和 uid,建议自己存 login_resp 的结果再分别 jq。
uid_of() {
  local resp uid
  resp=$(login_resp "$1")
  uid=$(echo "$resp" | jq -r '.data.user.id')
  [ "$uid" != "null" ] && [ -n "$uid" ] || fail "登录失败,拿不到 user_id: $resp"
  echo "$uid"
}

# ── docker 数据层断言辅助 ───────────────────────────────────────────────────

# mysql_q —— 在 dev MySQL 容器里跑一条 SQL,返回裸结果 (-N -B 去表头去格式)。
# 用法: CNT=$(mysql_q "SELECT COUNT(*) FROM follows WHERE follower_id=$UID")
mysql_q() {
  docker exec "$MYSQL_CONTAINER" mysql -uroot -p"$MYSQL_ROOT_PW" -N -B "$MYSQL_DB" -e "$1"
}

# redis_q —— 在 dev Redis 容器里跑一条 redis-cli 命令,去掉结尾的 \r 和引号。
# 用法: V=$(redis_q SISMEMBER "follow:set:$UID" "$TARGET")
redis_q() {
  docker exec "$REDIS_CONTAINER" redis-cli "$@" | tr -d '\r"'
}

# ── 断言函数库 ──────────────────────────────────────────────────────────────
# 所有 assert_* 失败时调用 fail (exit 1)。成功时调用 ok 打印绿勾。
# label 参数是人类可读的断言描述,会出现在通过/失败的输出里。

# assert_eq <expected> <actual> <label>
assert_eq() {
  if [ "$1" = "$2" ]; then
    ok "$3"
  else
    fail "$3 (期望 '$1',实际 '$2')"
  fi
}

# assert_ne <unexpected> <actual> <label>
assert_ne() {
  if [ "$1" != "$2" ]; then
    ok "$3"
  else
    fail "$3 (不应等于 '$1',但实际就是)"
  fi
}

# assert_ge <min> <actual> <label> —— 数值 >= 断言
assert_ge() {
  if [ "$2" -ge "$1" ] 2>/dev/null; then
    ok "$3"
  else
    fail "$3 (期望 >= $1,实际 '$2')"
  fi
}

# assert_nonempty <value> <label> —— 值非空且非 "null"
assert_nonempty() {
  if [ -n "$1" ] && [ "$1" != "null" ]; then
    ok "$2"
  else
    fail "$2 (值为空或 null)"
  fi
}

# assert_code <expected_code> <json_response> <label>
# 专门针对统一响应格式,内部 jq -r '.code'。最常用的 API 断言。
assert_code() {
  local actual
  actual=$(echo "$2" | jq -r '.code')
  if [ "$actual" = "$1" ]; then
    ok "$3"
  else
    fail "$3 (期望 code=$1,实际 code=$actual,响应: $2)"
  fi
}

# assert_json <json> <jq_filter> <expected> <label>
# 通用 jq 路径断言。用法:
#   assert_json "$RESP" '.data.has_more' 'true' "首页应有下一页"
#   assert_json "$RESP" '.data.posts | length' '2' "应命中 2 条"
assert_json() {
  local actual
  actual=$(echo "$1" | jq -r "$2")
  if [ "$actual" = "$3" ]; then
    ok "$4"
  else
    fail "$4 (jq '$2' 期望 '$3',实际 '$actual')"
  fi
}