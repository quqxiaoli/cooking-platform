#!/usr/bin/env bash
#
# scripts/verify_step6.sh — End-to-end verification for Step 6.
#
# Runs the 8 user-journey cases that prove Steps 3-5 are wired correctly:
#   1. Health & readiness
#   2. Auth full cycle (send-code → login → me → refresh)
#   3. Post create + Feed + Detail + Author page
#   4. Like / Unlike full cycle (Redis + MQ + MySQL)
#   5. CountConsumer redundant counter maintenance
#   6. Error code matrix (412104 / 401001 / 400001 / 412103)
#   7. Logout + JWT blacklist
#   8. Graceful shutdown (manual: Ctrl+C the server)
#
# Prerequisites:
#   - dev stack up: `make dev-up` or equivalent (MySQL + Redis on localhost)
#   - server running with stdout teed to logs/dev.log:
#       mkdir -p logs
#       CONFIG_PATH=configs/config.yaml go run ./cmd/server 2>&1 | tee logs/dev.log
#   - jq installed (brew install jq / apt install jq)
#   - docker containers named cooking-mysql-dev / cooking-redis-dev
#     (adjust names below if yours differ)

set -uo pipefail   # NOT -e: we want to keep going past failures and print summary

BASE="http://localhost:8080"
PHONE="13800138000"
LOG=logs/dev.log
PASS=0
FAIL=0

# ── Helpers ─────────────────────────────────────────────────────────────────

GREEN=$'\033[0;32m'
RED=$'\033[0;31m'
YELLOW=$'\033[1;33m'
RESET=$'\033[0m'

section() { printf "\n${YELLOW}════ %s ════${RESET}\n" "$1"; }
ok()      { printf "${GREEN}✓${RESET} %s\n" "$1"; PASS=$((PASS+1)); }
ko()      { printf "${RED}✗ %s${RESET}\n" "$1"; FAIL=$((FAIL+1)); }
note()    { printf "  %s\n" "$1"; }

# Pretty-print JSON or raw if jq fails
jqp() { echo "$1" | jq . 2>/dev/null || echo "$1"; }

# Pre-flight checks
[[ -f "$LOG" ]] || { echo "ERROR: $LOG missing. Start server with 'go run ./cmd/server 2>&1 | tee logs/dev.log'"; exit 1; }
command -v jq  >/dev/null || { echo "ERROR: jq not installed"; exit 1; }
command -v curl >/dev/null || { echo "ERROR: curl not installed"; exit 1; }

# ── 用例 1：健康检查 ─────────────────────────────────────────────────────────

section "用例 1 · 健康检查"

H_HEALTH=$(curl -s "$BASE/health")
note "GET /health → $H_HEALTH"
echo "$H_HEALTH" | grep -q '"status"' && ok "health 200" || ko "health failed"

H_READY=$(curl -s -w "\n%{http_code}" "$BASE/readiness")
H_READY_BODY=$(echo "$H_READY" | sed '$d')
H_READY_CODE=$(echo "$H_READY" | tail -n 1)
note "GET /readiness → HTTP $H_READY_CODE  body=$H_READY_BODY"
[[ "$H_READY_CODE" == "200" ]] && ok "readiness 200" || ko "readiness failed (HTTP $H_READY_CODE)"

# ── 用例 2：完整 auth 流程 ──────────────────────────────────────────────────

section "用例 2 · Auth 完整流程"

# (a) send-code
SEND_RESP=$(curl -s -X POST "$BASE/api/v1/auth/send-code" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE\"}")
note "POST /auth/send-code →"
jqp "$SEND_RESP" | sed 's/^/    /'
echo "$SEND_RESP" | grep -q '"code":0' && ok "send-code 成功" || ko "send-code 失败"

# (b) extract code from MockSender log
sleep 1
SMS_CODE=$(grep '\[SMS MOCK\]' "$LOG" | tail -1 | grep -oE '"code":[[:space:]]*"[0-9]+"' | grep -oE '[0-9]+')
[[ -n "$SMS_CODE" ]] && ok "从日志抓到验证码: $SMS_CODE" || { ko "日志中未发现验证码"; SMS_CODE="000000"; }

# (c) login
LOGIN_RESP=$(curl -s -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE\",\"code\":\"$SMS_CODE\"}")
note "POST /auth/login →"
jqp "$LOGIN_RESP" | sed 's/^/    /'
TOKEN=$(echo "$LOGIN_RESP" | jq -r '.data.access_token // empty')
USER_ID=$(echo "$LOGIN_RESP" | jq -r '.data.user.id // empty')
REFRESH=$(echo "$LOGIN_RESP" | jq -r '.data.refresh_token // empty')
[[ -n "$TOKEN" && -n "$USER_ID" ]] && ok "login 拿到 token (user_id=$USER_ID)" || { ko "login 失败"; exit 1; }

# (d) GET /users/me
ME_RESP=$(curl -s "$BASE/api/v1/users/me" -H "Authorization: Bearer $TOKEN")
note "GET /users/me →"
jqp "$ME_RESP" | sed 's/^/    /'
echo "$ME_RESP" | grep -q '"phone_masked"' && ok "/users/me 返回脱敏手机号" || ko "/users/me 失败"

# (e) refresh
REFRESH_RESP=$(curl -s -X POST "$BASE/api/v1/auth/refresh" \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}")
note "POST /auth/refresh →"
jqp "$REFRESH_RESP" | sed 's/^/    /'
NEW_TOKEN=$(echo "$REFRESH_RESP" | jq -r '.data.access_token // empty')
[[ -n "$NEW_TOKEN" ]] && ok "refresh 换出新 access_token" || ko "refresh 失败"

# ── 用例 3：发帖 + Feed + 详情 ──────────────────────────────────────────────

section "用例 3 · 发帖 / Feed / 详情 / 作者主页"

# Post 3 帖子，scene_tag 1/2/3
declare -a POST_IDS=()
for SCENE in 1 2 3; do
  RESP=$(curl -s -X POST "$BASE/api/v1/posts" \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -d "{\"title\":\"step6-verify-scene$SCENE\",\"scene_tag\":$SCENE,\"content\":\"end-to-end test post for scene $SCENE\"}")
  PID=$(echo "$RESP" | jq -r '.data.post_id // .data.id // empty')
  if [[ -n "$PID" && "$PID" != "null" ]]; then
    ok "发帖 scene=$SCENE → post_id=$PID"
    POST_IDS+=("$PID")
  else
    ko "发帖 scene=$SCENE 失败：$(jqp "$RESP")"
  fi
done

if [[ ${#POST_IDS[@]} -eq 0 ]]; then
  ko "三次发帖全部失败，终止后续用例"; exit 1
fi
LATEST_ID=${POST_IDS[$((${#POST_IDS[@]}-1))]}
note "最近一篇 post_id=$LATEST_ID"
# Feed 全量
sleep 1   # 给 PostEvent 一点时间被消费
FEED_ALL=$(curl -s "$BASE/api/v1/feed?size=10")
note "GET /feed?size=10 →"
jqp "$FEED_ALL" | sed 's/^/    /' | head -50
FEED_COUNT=$(echo "$FEED_ALL" | jq '.data.posts | length')
[[ "$FEED_COUNT" -ge 3 ]] && ok "Feed 返回 ≥3 条 (实际 $FEED_COUNT)" || ko "Feed 数量不足 ($FEED_COUNT)"

# Feed 按场景过滤
FEED_S2=$(curl -s "$BASE/api/v1/feed?scene_tag=2&size=10")
note "GET /feed?scene_tag=2 →"
jqp "$FEED_S2" | sed 's/^/    /' | head -30
S2_COUNT=$(echo "$FEED_S2" | jq '[.data.posts[] | select(.scene_tag==2)] | length')
[[ "$S2_COUNT" -ge 1 ]] && ok "scene_tag=2 过滤生效 (返回 $S2_COUNT 条 scene=2)" || ko "scene_tag 过滤失效"

# Cursor 翻页
PAGE1=$(curl -s "$BASE/api/v1/feed?size=2")
CURSOR=$(echo "$PAGE1" | jq -r '.data.next_cursor // empty')
note "第一页 next_cursor=$CURSOR"
if [[ -n "$CURSOR" && "$CURSOR" != "null" ]]; then
  PAGE2=$(curl -s "$BASE/api/v1/feed?size=2&cursor=$CURSOR")
  note "GET /feed?size=2&cursor=$CURSOR →"
  jqp "$PAGE2" | sed 's/^/    /' | head -30
  ok "游标翻页可达"
else
  note "next_cursor 空（数据不够 2 页），跳过翻页验证"
fi

# 详情页
DETAIL=$(curl -s "$BASE/api/v1/posts/$LATEST_ID")
note "GET /posts/$LATEST_ID →"
jqp "$DETAIL" | sed 's/^/    /'
DT_TITLE=$(echo "$DETAIL" | jq -r '.data.title // empty')
[[ -n "$DT_TITLE" ]] && ok "详情页返回 (title=$DT_TITLE)" || ko "详情页失败"

# 作者主页
AUTHOR_PAGE=$(curl -s "$BASE/api/v1/users/$USER_ID/posts")
note "GET /users/$USER_ID/posts →"
jqp "$AUTHOR_PAGE" | sed 's/^/    /' | head -40
AUTHOR_COUNT=$(echo "$AUTHOR_PAGE" | jq '.data.posts | length')
[[ "$AUTHOR_COUNT" -ge 3 ]] && ok "作者主页返回 ≥3 篇 (实际 $AUTHOR_COUNT)" || ko "作者主页篇数异常"

# ── 用例 4：点赞链路 ──────────────────────────────────────────────────────

section "用例 4 · 点赞 / 取消 / Redis-MQ-MySQL 全链路"

# 点赞
LIKE_RESP=$(curl -s -X POST "$BASE/api/v1/posts/$LATEST_ID/like" -H "Authorization: Bearer $TOKEN")
note "POST /posts/$LATEST_ID/like →"
jqp "$LIKE_RESP" | sed 's/^/    /'
LIKED=$(echo "$LIKE_RESP" | jq -r '.data.liked')
COUNT=$(echo "$LIKE_RESP" | jq -r '.data.count')
[[ "$LIKED" == "true" && "$COUNT" == "1" ]] && ok "点赞 liked=true count=1" || ko "点赞返回异常 (liked=$LIKED count=$COUNT)"

# 状态查询
STATUS_RESP=$(curl -s "$BASE/api/v1/posts/$LATEST_ID/like" -H "Authorization: Bearer $TOKEN")
note "GET /posts/$LATEST_ID/like →"
jqp "$STATUS_RESP" | sed 's/^/    /'

# Redis 即时反馈
echo
note "── Redis 即时状态 ──"
SISMEMBER=$(docker exec cooking-redis-dev redis-cli SISMEMBER "like:set:$LATEST_ID" "$USER_ID")
LIKE_CNT=$(docker exec cooking-redis-dev redis-cli GET "like:cnt:$LATEST_ID")
note "SISMEMBER like:set:$LATEST_ID $USER_ID = $SISMEMBER"
note "GET like:cnt:$LATEST_ID = $LIKE_CNT"
[[ "$SISMEMBER" == "1" && "$LIKE_CNT" == "1" ]] && ok "Redis SET + 计数器即时一致" || ko "Redis 状态异常"

# 等 4 秒落库
echo
note "等待 4s 让 LikeConsumer 批量落库..."
sleep 4
MYSQL_LIKE_CNT=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT like_count FROM posts WHERE id=$LATEST_ID" 2>/dev/null)
MYSQL_LIKE_ROWS=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT COUNT(*) FROM likes WHERE post_id=$LATEST_ID AND user_id=$USER_ID" 2>/dev/null)
note "posts.like_count = $MYSQL_LIKE_CNT"
note "likes WHERE post=$LATEST_ID AND user=$USER_ID 行数 = $MYSQL_LIKE_ROWS"
[[ "$MYSQL_LIKE_CNT" == "1" && "$MYSQL_LIKE_ROWS" == "1" ]] && ok "MySQL 落库一致" || ko "MySQL 落库异常"

# 取消点赞
UNLIKE_RESP=$(curl -s -X DELETE "$BASE/api/v1/posts/$LATEST_ID/like" -H "Authorization: Bearer $TOKEN")
note "DELETE /posts/$LATEST_ID/like →"
jqp "$UNLIKE_RESP" | sed 's/^/    /'
UNLIKED=$(echo "$UNLIKE_RESP" | jq -r '.data.liked')
[[ "$UNLIKED" == "false" ]] && ok "取消点赞 liked=false" || ko "取消点赞返回异常"

# Redis 状态归零
SISMEMBER2=$(docker exec cooking-redis-dev redis-cli SISMEMBER "like:set:$LATEST_ID" "$USER_ID")
LIKE_CNT2=$(docker exec cooking-redis-dev redis-cli GET "like:cnt:$LATEST_ID")
note "取消后 SISMEMBER=$SISMEMBER2, like:cnt=$LIKE_CNT2"
[[ "$SISMEMBER2" == "0" && "$LIKE_CNT2" == "0" ]] && ok "Redis 状态正确归零" || ko "Redis 未归零"

# 等 4 秒确认 MySQL 同步
sleep 4
MYSQL_LIKE_CNT2=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT like_count FROM posts WHERE id=$LATEST_ID" 2>/dev/null)
MYSQL_LIKE_ROWS2=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT COUNT(*) FROM likes WHERE post_id=$LATEST_ID AND user_id=$USER_ID" 2>/dev/null)
note "取消后 posts.like_count = $MYSQL_LIKE_CNT2, likes 行数 = $MYSQL_LIKE_ROWS2"
[[ "$MYSQL_LIKE_CNT2" == "0" && "$MYSQL_LIKE_ROWS2" == "0" ]] && ok "MySQL 同步归零" || ko "MySQL 未归零"

# ── 用例 5：CountConsumer 维护 users 冗余计数 ────────────────────────────────

section "用例 5 · CountConsumer 维护 users 冗余计数"

note "等待 11s 让 CountConsumer 完成一个周期 (10s/批)..."
sleep 11
USER_COUNTS=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT id, post_count, total_likes FROM users WHERE id=$USER_ID" 2>/dev/null)
note "users WHERE id=$USER_ID → $USER_COUNTS"
PC=$(echo "$USER_COUNTS" | awk '{print $2}')
TL=$(echo "$USER_COUNTS" | awk '{print $3}')
[[ "$PC" -ge 3 ]] && ok "post_count=$PC ≥ 3 (CountConsumer 工作正常)" || ko "post_count=$PC 异常"
note "total_likes=$TL (本测试中点了又取消，预期 0；非 0 也合理如果之前有残留)"

# ── 用例 6：错误码全量验证 ──────────────────────────────────────────────────

section "用例 6 · 错误码矩阵"

# 412104 帖子不存在
ERR1=$(curl -s -X POST "$BASE/api/v1/posts/999999/like" -H "Authorization: Bearer $TOKEN")
note "POST /posts/999999/like → $ERR1"
echo "$ERR1" | grep -q '"code":412104' && ok "412104 帖子不存在" || ko "412104 未触发"

# 401001 未登录
ERR2=$(curl -s -X POST "$BASE/api/v1/posts/$LATEST_ID/like")
note "POST /posts/:id/like (无 Authorization) → $ERR2"
echo "$ERR2" | grep -q '"code":401001' && ok "401001 未登录" || ko "401001 未触发"

# 400001 + 412103 参数校验
ERR3=$(curl -s -X POST "$BASE/api/v1/posts" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title":"","scene_tag":99}')
note "POST /posts (空标题 + 非法 scene) → $ERR3"
echo "$ERR3" | grep -qE '"code":(400001|412101|412103)' && ok "参数校验错误码命中" || ko "参数校验未触发"

# ── 用例 7：logout + JWT 黑名单 ─────────────────────────────────────────────

section "用例 7 · Logout + JWT 黑名单"

LOGOUT_RESP=$(curl -s -X POST "$BASE/api/v1/auth/logout" -H "Authorization: Bearer $TOKEN")
note "POST /auth/logout → $LOGOUT_RESP"
echo "$LOGOUT_RESP" | grep -q '"code":0' && ok "logout 成功" || ko "logout 失败"

# 用同 token 调受保护接口
AFTER_LOGOUT=$(curl -s "$BASE/api/v1/users/me" -H "Authorization: Bearer $TOKEN")
note "logout 后 GET /users/me (用旧 token) → $AFTER_LOGOUT"
echo "$AFTER_LOGOUT" | grep -qE '"code":401(001|003)' && ok "黑名单生效，旧 token 被拒" || ko "黑名单未生效"

# ── 用例 8：优雅停机（手动）────────────────────────────────────────────────

section "用例 8 · 优雅停机"
note "请到运行 server 的终端按 Ctrl+C，然后回车继续..."
read -r _

note "── 优雅停机日志末尾 30 行 ──"
tail -30 "$LOG"

# ── 总结 ────────────────────────────────────────────────────────────────────

section "验证总结"
TOTAL=$((PASS+FAIL))
printf "通过 %s${GREEN}%d${RESET}%s / 失败 %s${RED}%d${RESET}%s / 共 %d\n" \
  "" "$PASS" "" "" "$FAIL" "" "$TOTAL"

if [[ $FAIL -eq 0 ]]; then
  printf "${GREEN}━━━ 全链路验证通过 ━━━${RESET}\n"
else
  printf "${RED}━━━ 有用例失败，定位修复后重跑 ━━━${RESET}\n"
  exit 1
fi