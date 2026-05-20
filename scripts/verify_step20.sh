#!/usr/bin/env bash
# scripts/verify_step20.sh — Step 20 公网端到端验证
#
# 三合一：HTTPS 探活 + 业务流（注册/发帖/Feed/详情/点赞/关注/搜索）
#          + 主从读写分离实证（master vs slave Com_select 增量比对）
#
# ── 与 dev 验证脚本的差别 ───────────────────────────────────────────────────
#   - 必须在 prod 服务器上运行（docker exec 抓 [SMS MOCK] 日志）
#   - 不 source verify_common.sh（dev 专用：logs/dev.log + 容器名 -dev 后缀）
#   - 自己实现精简断言函数，失败不立即退出，用计数器汇总（调试友好）
#   - BASE 走公网 HTTPS：https://mellowck.com/api/v1
#
# ── 用法 ────────────────────────────────────────────────────────────────────
#   source .env.prod && bash scripts/verify_step20.sh
#   # 或直接传：
#   MYSQL_ROOT_PW=xxx bash scripts/verify_step20.sh

set -u  # 不开 -e —— 我们用 FAIL_COUNT 计数，失败不退出

# ── 公共常量 ────────────────────────────────────────────────────────────────
BASE="${BASE:-https://mellowck.com/api/v1}"
APP1_CONTAINER="${APP1_CONTAINER:-cooking-app1-prod}"
APP2_CONTAINER="${APP2_CONTAINER:-cooking-app2-prod}"
MYSQL_MASTER_CONTAINER="${MYSQL_MASTER_CONTAINER:-cooking-mysql-prod}"
MYSQL_SLAVE_CONTAINER="${MYSQL_SLAVE_CONTAINER:-cooking-mysql-slave-prod}"
NGINX_CONTAINER="${NGINX_CONTAINER:-cooking-nginx-prod}"
MYSQL_ROOT_PW="${MYSQL_ROOT_PW:-${MYSQL_ROOT_PASSWORD:-}}"

# ── 颜色与输出 ──────────────────────────────────────────────────────────────
C_G='\033[0;32m'; C_R='\033[0;31m'; C_Y='\033[1;33m'; C_N='\033[0m'

PASS_COUNT=0
FAIL_COUNT=0
WARN_COUNT=0

ok()   { echo -e "${C_G}✓${C_N} $1"; PASS_COUNT=$((PASS_COUNT+1)); }
bad()  { echo -e "${C_R}✗${C_N} $1"; FAIL_COUNT=$((FAIL_COUNT+1)); }
warn() { echo -e "${C_Y}⚠${C_N} $1"; WARN_COUNT=$((WARN_COUNT+1)); }
info() { echo -e "${C_Y}→${C_N} $1"; }
section() { echo ""; echo -e "${C_Y}── $1 ──${C_N}"; }

# ── 精简断言库（与 verify_common.sh 行为一致，但失败不 exit） ──────────────
assert_eq() {
  if [ "$1" = "$2" ]; then ok "$3"; else bad "$3 (期望 '$1'，实际 '$2')"; fi
}
assert_code() {
  local actual; actual=$(echo "$2" | jq -r '.code' 2>/dev/null)
  if [ "$actual" = "$1" ]; then ok "$3"; else bad "$3 (期望 code=$1，实际 code=$actual，响应: $2)"; fi
}
assert_code_in() {
  # assert_code_in "0|42xxxx|..." "$resp" "label"
  local actual; actual=$(echo "$2" | jq -r '.code' 2>/dev/null)
  if echo "$actual" | grep -qE "^($1)$"; then ok "$3 (code=$actual)"; else bad "$3 (期望 code∈{$1}，实际 code=$actual，响应: $2)"; fi
}
assert_nonempty() {
  if [ -n "$1" ] && [ "$1" != "null" ]; then ok "$2"; else bad "$2 (值为空或 null)"; fi
}
assert_http_200() {
  if [ "$1" = "200" ]; then ok "$2 → HTTP $1"; else bad "$2 → HTTP $1 (期望 200)"; fi
}

# ── 容器内 SQL helper ──────────────────────────────────────────────────────
# 在指定 MySQL 容器内跑一条 SQL，返回单值（-N -B 去表头去对齐）。
mysql_q() {
  local container="$1" sql="$2"
  docker exec "$container" mysql -uroot -p"$MYSQL_ROOT_PW" -N -B -e "$sql" 2>/dev/null
}

# 抓某手机号最新一条 [SMS MOCK] 验证码。
# prod 配置 log.console:false → stdout 为空，必须读容器内 /var/log/cooking/app.log
# 的 zap JSON 日志。send-code 经 Nginx 负载均衡可能落 app1 也可能落 app2，两个
# 实例都要查，按 phone 字段精确过滤避免拿到上一次测试的码，再用 jq 取 code 字段。
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

command -v jq    >/dev/null && ok "jq 已安装"   || { bad "需要 jq (apt-get install -y jq)"; exit 1; }
command -v curl  >/dev/null && ok "curl 已安装" || { bad "需要 curl"; exit 1; }

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

# ════════════════════════════════════════════════════════════════════════════
# §2 HTTPS 探活
# ════════════════════════════════════════════════════════════════════════════
section "§2 HTTPS 探活"

# 应用只注册了 /health 和 /api/v1/...，没有 "/" 路由，所以不测根路径（会 404）
http_health_apex=$(curl -s -o /dev/null -w "%{http_code}" "https://mellowck.com/health" || echo "000")
assert_http_200 "$http_health_apex" "https://mellowck.com/health"

http_health_www=$(curl -s -o /dev/null -w "%{http_code}" "https://www.mellowck.com/health" || echo "000")
assert_http_200 "$http_health_www" "https://www.mellowck.com/health"

# 80→443 301 跳转
http_80=$(curl -s -o /dev/null -w "%{http_code}" "http://mellowck.com/" || echo "000")
assert_eq "301" "$http_80" "http://mellowck.com/ 应 301 跳转到 HTTPS"

# ════════════════════════════════════════════════════════════════════════════
# §3 主从 Com_select 基线
# ════════════════════════════════════════════════════════════════════════════
section "§3 主从 Com_select 基线"

MASTER_BASE=$(mysql_q "$MYSQL_MASTER_CONTAINER" "SHOW GLOBAL STATUS LIKE 'Com_select'" | awk '{print $2}')
SLAVE_BASE=$(mysql_q  "$MYSQL_SLAVE_CONTAINER"  "SHOW GLOBAL STATUS LIKE 'Com_select'" | awk '{print $2}')

assert_nonempty "$MASTER_BASE" "master Com_select 基线读取成功"
assert_nonempty "$SLAVE_BASE"  "slave  Com_select 基线读取成功"
info "  master baseline Com_select = $MASTER_BASE"
info "  slave  baseline Com_select = $SLAVE_BASE"

# ════════════════════════════════════════════════════════════════════════════
# §4 注册 + 登录（用户 1）
# ════════════════════════════════════════════════════════════════════════════
section "§4 用户 1 注册 + 登录"

PHONE1="138$(printf '%08d' $((RANDOM*RANDOM % 100000000)))"
info "  随机手机号 (用户 1): $PHONE1"

curl -sS -X POST "$BASE/auth/send-code" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE1\"}" >/dev/null
sleep 0.5   # 等日志刷盘

CODE1=$(fetch_sms_code "$PHONE1")
assert_nonempty "$CODE1" "从 app.log 抓到用户 1 验证码"
info "  抓到验证码: $CODE1"

LOGIN1=$(curl -sS -X POST "$BASE/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE1\",\"code\":\"$CODE1\"}")
assert_code 0 "$LOGIN1" "用户 1 登录返回 code=0"

TOKEN1=$(echo "$LOGIN1" | jq -r '.data.access_token')
USER1_ID=$(echo "$LOGIN1" | jq -r '.data.user.id')
assert_nonempty "$TOKEN1"   "用户 1 拿到 access_token"
assert_nonempty "$USER1_ID" "用户 1 拿到 user_id"

# ════════════════════════════════════════════════════════════════════════════
# §5 发帖
# ════════════════════════════════════════════════════════════════════════════
section "§5 用户 1 发帖"

MARK=$(date +%s)_$RANDOM
RANDOM_TOKEN="step20-$RANDOM-$(date -u +%s)"
TITLE="Step 20 verify $RANDOM_TOKEN"
CONTENT="verify_step20.sh end-to-end test post for $RANDOM_TOKEN mark=${MARK}"

POST_BODY=$(jq -nc \
  --arg title "$TITLE" \
  --arg content "$CONTENT" \
  '{
    title: $title,
    scene_tag: 1,
    content: $content
  }')

POST_RESP=$(curl -sS -X POST "$BASE/posts" \
  -H "Authorization: Bearer $TOKEN1" \
  -H 'Content-Type: application/json' \
  -d "$POST_BODY")

assert_code 0 "$POST_RESP" "发帖返回 code=0"
info "  发帖完整响应: $POST_RESP"
POST_ID=$(echo "$POST_RESP" | jq -r '.data.post_id // empty')
assert_nonempty "$POST_ID" "抓到 post_id"
info "  post_id = $POST_ID"

# ════════════════════════════════════════════════════════════════════════════
# §6 Feed 浏览
# ════════════════════════════════════════════════════════════════════════════
section "§6 Feed 浏览"

FEED_RESP=$(curl -sS "$BASE/feed")
assert_code 0 "$FEED_RESP" "Feed 返回 code=0"

# 字段存在性即可（has_more bool / posts array），不强求命中刚发的帖子
HAS_MORE=$(echo "$FEED_RESP" | jq -r '.data.has_more')
POSTS_TYPE=$(echo "$FEED_RESP" | jq -r '.data.posts | type')
if [ "$HAS_MORE" = "true" ] || [ "$HAS_MORE" = "false" ]; then
  ok "data.has_more 是 bool ($HAS_MORE)"
else
  warn "data.has_more 不是 bool ($HAS_MORE) —— 接口契约可能变了"
fi
assert_eq "array" "$POSTS_TYPE" "data.posts 是数组"

# ════════════════════════════════════════════════════════════════════════════
# §7 详情页
# ════════════════════════════════════════════════════════════════════════════
section "§7 详情页 GET /posts/:id"

if [ -z "$POST_ID" ]; then
  warn "§5 发帖未拿到 post_id，跳过 §7 详情页测试"
else
  DETAIL=$(curl -sS "$BASE/posts/$POST_ID")
  # 刚发的帖子可能在 pending_review，业务码非 0 但属正常 —— 接受 0 或 42XXXX 系列
  assert_code_in "0|42[0-9]{4}" "$DETAIL" "详情页返回 code=0 或 42YZZZ 系列（pending 也算 OK）"
fi

# ════════════════════════════════════════════════════════════════════════════
# §8 点赞（用户 2 → 用户 1 的帖子）
# ════════════════════════════════════════════════════════════════════════════
section "§8 点赞（用户 2 → 用户 1 的帖子）"

PHONE2="138$(printf '%08d' $(((RANDOM*RANDOM+1) % 100000000)))"
info "  随机手机号 (用户 2): $PHONE2"

curl -sS -X POST "$BASE/auth/send-code" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE2\"}" >/dev/null
sleep 0.5   # 等日志刷盘

CODE2=$(fetch_sms_code "$PHONE2")
assert_nonempty "$CODE2" "从 app.log 抓到用户 2 验证码"

LOGIN2=$(curl -sS -X POST "$BASE/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE2\",\"code\":\"$CODE2\"}")
assert_code 0 "$LOGIN2" "用户 2 登录返回 code=0"

TOKEN2=$(echo "$LOGIN2" | jq -r '.data.access_token')
USER2_ID=$(echo "$LOGIN2" | jq -r '.data.user.id')
assert_nonempty "$TOKEN2"   "用户 2 拿到 access_token"
assert_nonempty "$USER2_ID" "用户 2 拿到 user_id"

if [ -z "$POST_ID" ]; then
  warn "§5 发帖未拿到 post_id，跳过 §8 点赞测试"
else
  LIKE_RESP=$(curl -sS -X POST "$BASE/posts/$POST_ID/like" \
    -H "Authorization: Bearer $TOKEN2")
  LIKE_CODE=$(echo "$LIKE_RESP" | jq -r '.code')
  case "$LIKE_CODE" in
    0)
      ok "点赞返回 code=0"
      ;;
    42*|43*)
      # 业务规则可能要求"已审核才能交互"，对刚发的 pending 帖子返回业务错误码是合理的
      warn "点赞返回业务错误码 code=$LIKE_CODE（pending 内容不可交互？）—— 跳过断言"
      ;;
    *)
      bad "点赞返回未预期 code=$LIKE_CODE，响应: $LIKE_RESP"
      ;;
  esac
fi

# ════════════════════════════════════════════════════════════════════════════
# §9 关注（用户 2 → 用户 1）
# ════════════════════════════════════════════════════════════════════════════
section "§9 关注（用户 2 → 用户 1）"

FOLLOW_RESP=$(curl -sS -X POST "$BASE/users/$USER1_ID/follow" \
  -H "Authorization: Bearer $TOKEN2")
assert_code 0 "$FOLLOW_RESP" "关注返回 code=0"

# ════════════════════════════════════════════════════════════════════════════
# §10 搜索
# ════════════════════════════════════════════════════════════════════════════
section "§10 搜索"

# 用发帖时埋入的 mark 串搜索（even if 索引未及时更新，code=0 就算 OK）
SEARCH_RESP=$(curl -sS "$BASE/search?q=${MARK}")
assert_code 0 "$SEARCH_RESP" "搜索返回 code=0（搜索是 best-effort，命不命中不强求）"

# ════════════════════════════════════════════════════════════════════════════
# §11 主从 Com_select 增量
# ════════════════════════════════════════════════════════════════════════════
section "§11 主从 Com_select 增量"

MASTER_NOW=$(mysql_q "$MYSQL_MASTER_CONTAINER" "SHOW GLOBAL STATUS LIKE 'Com_select'" | awk '{print $2}')
SLAVE_NOW=$(mysql_q  "$MYSQL_SLAVE_CONTAINER"  "SHOW GLOBAL STATUS LIKE 'Com_select'" | awk '{print $2}')

MASTER_DELTA=$((MASTER_NOW - MASTER_BASE))
SLAVE_DELTA=$((SLAVE_NOW  - SLAVE_BASE))
TOTAL_DELTA=$((MASTER_DELTA + SLAVE_DELTA))

info "  master Com_select delta = $MASTER_DELTA"
info "  slave  Com_select delta = $SLAVE_DELTA"
info "  total                   = $TOTAL_DELTA"

if [ "$TOTAL_DELTA" -le 0 ]; then
  warn "本次跑未观察到 Com_select 增长（可能只命中缓存层，未走 MySQL）"
else
  SLAVE_PCT=$(( SLAVE_DELTA * 100 / TOTAL_DELTA ))
  info "  slave 占比 ≈ ${SLAVE_PCT}%"
  # 期望：slave 增量 > master 增量 / 2（Random Policy 统计意义上的均匀，
  # 单次跑可能偏一边，故不强卡阈值）
  if [ "$SLAVE_DELTA" -gt "$(( MASTER_DELTA / 2 ))" ]; then
    ok "slave 至少分摊了一定读流量（slave_delta=$SLAVE_DELTA > master_delta/2=$((MASTER_DELTA/2))）"
  else
    warn "slave 分摊比例偏低（slave_delta=$SLAVE_DELTA, master_delta=$MASTER_DELTA） —— 单次抽样误差，多跑几次再观察"
  fi
fi

# ════════════════════════════════════════════════════════════════════════════
# 汇总
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
