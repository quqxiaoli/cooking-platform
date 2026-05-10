#!/usr/bin/env bash
# scripts/verify_step5.sh
# Step 5 端到端验证：自动跑通 send-code → login → 发帖 → 点赞全链路。
# 验证码从 logs/dev.log 中抓取（make run 须用 `2>&1 | tee logs/dev.log` 启动）。

set -e

BASE="http://localhost:8080/api/v1"
PHONE="13800138000"
LOG_FILE="logs/dev.log"

# 颜色
G='\033[0;32m'; R='\033[0;31m'; Y='\033[1;33m'; N='\033[0m'
ok()   { echo -e "${G}✓${N} $1"; }
fail() { echo -e "${R}✗${N} $1"; exit 1; }
info() { echo -e "${Y}→${N} $1"; }

# 前置检查
[ -f "$LOG_FILE" ] || fail "日志文件 $LOG_FILE 不存在，请用 'go run ./cmd/server 2>&1 | tee logs/dev.log' 启动服务"
command -v jq >/dev/null || fail "需要 jq 命令"

# ── 1. send-code ───────────────────────────────────────────────────────────
info "1. 请求验证码"
RESP=$(curl -s -X POST "$BASE/auth/send-code" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE\"}")
CODE=$(echo "$RESP" | jq -r '.code')
if [ "$CODE" = "0" ]; then
  ok "send-code 成功"
elif [ "$CODE" = "410105" ]; then
  info "60s 短窗限流，使用上次的验证码"
else
  fail "send-code 失败: $RESP"
fi

# ── 2. 从日志抓最新验证码 ──────────────────────────────────────────────────
info "2. 从日志中抓验证码"
sleep 0.3   # 等日志落盘
SMS_CODE=$(grep '\[SMS MOCK\]' "$LOG_FILE" | tail -1 | grep -oE '"code": "[0-9]+"' | grep -oE '[0-9]+')
[ -n "$SMS_CODE" ] || fail "日志中找不到验证码（grep '[SMS MOCK]' $LOG_FILE）"
ok "抓到验证码: $SMS_CODE"

# ── 3. login ───────────────────────────────────────────────────────────────
info "3. 登录"
LOGIN_RESP=$(curl -s -X POST "$BASE/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PHONE\",\"code\":\"$SMS_CODE\"}")
TOKEN=$(echo "$LOGIN_RESP" | jq -r '.data.access_token')
USER_ID=$(echo "$LOGIN_RESP" | jq -r '.data.user.id')
DOTS=$(echo -n "$TOKEN" | tr -cd '.' | wc -c | tr -d ' ')
[ "$DOTS" = "2" ] || fail "token 格式错误（点数=$DOTS，应为 2）: $LOGIN_RESP"
ok "登录成功，user_id=$USER_ID"

# ── 4. 发帖 ────────────────────────────────────────────────────────────────
info "4. 发一篇被点赞的测试帖"
POST_RESP=$(curl -s -X POST "$BASE/posts" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title":"被点赞测试","scene_tag":1,"content":"step5 verify"}')
POST_ID=$(echo "$POST_RESP" | jq -r '.data.post_id')
[ "$POST_ID" != "null" ] && [ -n "$POST_ID" ] || fail "发帖失败: $POST_RESP"
ok "发帖成功，post_id=$POST_ID"

# ── 5. 点赞 ────────────────────────────────────────────────────────────────
info "5-1. 第一次点赞（POST /posts/$POST_ID/like）"
LIKE1=$(curl -s -X POST "$BASE/posts/$POST_ID/like" -H "Authorization: Bearer $TOKEN")
LIKED=$(echo "$LIKE1" | jq -r '.data.liked')
COUNT=$(echo "$LIKE1" | jq -r '.data.count')
[ "$LIKED" = "true" ] && [ "$COUNT" = "1" ] || fail "点赞响应异常: $LIKE1"
ok "liked=true count=1"

# ── 6. Redis 即时状态 ──────────────────────────────────────────────────────
info "5-2. 检查 Redis 即时状态"
SISMEMBER=$(docker exec cooking-mysql-dev sh -c "true" 2>/dev/null && echo skip || echo skip)
SETMEMBER=$(docker exec cooking-redis-dev redis-cli SISMEMBER "like:set:$POST_ID" "$USER_ID" | tr -d '\r')
LIKECNT=$(docker exec cooking-redis-dev redis-cli GET "like:cnt:$POST_ID" | tr -d '\r"')
[ "$SETMEMBER" = "1" ] || fail "Redis SISMEMBER 期望 1, 实际 $SETMEMBER"
[ "$LIKECNT" = "1" ]  || fail "Redis like:cnt 期望 1, 实际 $LIKECNT"
ok "Redis like:set=1 like:cnt=1"

# ── 7. 重复点赞幂等 ────────────────────────────────────────────────────────
info "5-3. 重复点赞（应幂等，count 仍为 1）"
LIKE2=$(curl -s -X POST "$BASE/posts/$POST_ID/like" -H "Authorization: Bearer $TOKEN")
COUNT2=$(echo "$LIKE2" | jq -r '.data.count')
[ "$COUNT2" = "1" ] || fail "幂等失败，count=$COUNT2"
ok "幂等正常，count 仍为 1"

# ── 8. 等 LikeConsumer 落库 ────────────────────────────────────────────────
info "5-4. 等 4 秒让 LikeConsumer 落库"
sleep 4
LIKES_ROWS=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT COUNT(*) FROM likes WHERE post_id=$POST_ID")
LIKE_CNT_DB=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT like_count FROM posts WHERE id=$POST_ID")
[ "$LIKES_ROWS" = "1" ] || fail "likes 表期望 1 行, 实际 $LIKES_ROWS"
[ "$LIKE_CNT_DB" = "1" ] || fail "posts.like_count 期望 1, 实际 $LIKE_CNT_DB"
ok "MySQL likes=1 行，posts.like_count=1"

# ── 9. 取消点赞 ────────────────────────────────────────────────────────────
info "5-5. 取消点赞（DELETE /posts/$POST_ID/like）"
UNLIKE=$(curl -s -X DELETE "$BASE/posts/$POST_ID/like" -H "Authorization: Bearer $TOKEN")
LIKED3=$(echo "$UNLIKE" | jq -r '.data.liked')
COUNT3=$(echo "$UNLIKE" | jq -r '.data.count')
[ "$LIKED3" = "false" ] && [ "$COUNT3" = "0" ] || fail "取消点赞响应异常: $UNLIKE"
ok "liked=false count=0"

SISMEMBER2=$(docker exec cooking-redis-dev redis-cli SISMEMBER "like:set:$POST_ID" "$USER_ID" | tr -d '\r')
LIKECNT2=$(docker exec cooking-redis-dev redis-cli GET "like:cnt:$POST_ID" | tr -d '\r"')
[ "$SISMEMBER2" = "0" ] || fail "Redis SISMEMBER 期望 0, 实际 $SISMEMBER2"
[ "$LIKECNT2" = "0" ]  || fail "Redis like:cnt 期望 0, 实际 $LIKECNT2"
ok "Redis 状态已清: SISMEMBER=0 like:cnt=0"

info "5-6. 等 4 秒让 LikeConsumer 落库取消"
sleep 4
LIKES_ROWS2=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT COUNT(*) FROM likes WHERE post_id=$POST_ID")
LIKE_CNT_DB2=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT like_count FROM posts WHERE id=$POST_ID")
[ "$LIKES_ROWS2" = "0" ] || fail "likes 表期望 0 行, 实际 $LIKES_ROWS2"
[ "$LIKE_CNT_DB2" = "0" ] || fail "posts.like_count 期望 0, 实际 $LIKE_CNT_DB2"
ok "MySQL likes=0 行, posts.like_count=0"

# ── 10. PVConsumer 验证 ───────────────────────────────────────────────────
info "6-1. 触发 3 次详情访问（PV 去重后只算 1 次）"
for i in 1 2 3; do
  curl -s "$BASE/posts/$POST_ID?nocache=$i" -H "Authorization: Bearer $TOKEN" >/dev/null
done
sleep 6
VIEW_CNT=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT view_count FROM posts WHERE id=$POST_ID")
[ "$VIEW_CNT" = "1" ] || fail "view_count 期望 1（PV 去重）, 实际 $VIEW_CNT"
ok "PVConsumer 落库正常，view_count=1"

# ── 11. CountConsumer 验证 ────────────────────────────────────────────────
info "7-1. 检查 users.post_count 是否被 CountConsumer 维护"
POST_CNT=$(docker exec cooking-mysql-dev mysql -uroot -pcooking123 -N -B cooking_platform \
  -e "SELECT post_count FROM users WHERE id=$USER_ID")
# 第 5 步上线后只能从此刻开始计数（第 4 步发的帖子的 PostEvent 已被 drop）
# 本次脚本发了 1 篇，所以期望 ≥ 1（之前的 step5 验证可能也跑过几次）
[ "$POST_CNT" -ge 1 ] || fail "users.post_count 期望 ≥1, 实际 $POST_CNT"
ok "users.post_count=$POST_CNT（≥1，CountConsumer 工作正常）"

# ── 12. 错误用例 ──────────────────────────────────────────────────────────
info "8-1. 给不存在的帖子点赞 → 412104"
ERR1=$(curl -s -X POST "$BASE/posts/999999/like" -H "Authorization: Bearer $TOKEN" | jq -r '.code')
[ "$ERR1" = "412104" ] || fail "期望 412104, 实际 $ERR1"
ok "412104 正确"

info "8-2. 未登录点赞 → 401001"
ERR2=$(curl -s -X POST "$BASE/posts/$POST_ID/like" | jq -r '.code')
[ "$ERR2" = "401001" ] || fail "期望 401001, 实际 $ERR2"
ok "401001 正确"

echo ""
echo -e "${G}========================================${N}"
echo -e "${G}  Step 5 端到端验证全部通过 ✓${N}"
echo -e "${G}========================================${N}"
echo ""
echo "现在去 make run 终端按 Ctrl+C，观察优雅停机日志："
echo "  应包含 6 行 drain 日志（http server stopped → 3 条 drained → consumer manager stopped → server exited cleanly）"
