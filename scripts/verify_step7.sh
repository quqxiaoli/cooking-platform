#!/usr/bin/env bash
# scripts/verify_step7.sh
# Step 7 端到端验证：搜索模块全链路 + R-1 修复回归。
#
# 覆盖：
#   1. R-1 回归 —— 首次登录(自动注册)响应里 user.created_at 不再是零值
#   2. 发 3 篇标题可控的测试帖，喂给 FULLTEXT 索引
#   3. 关键词搜索命中 + 相关度排序
#   4. scene_tag 服务端过滤
#   5. offset 游标翻页 (cursor / next_cursor / has_more)
#   6. 错误用例：空关键词 450101、非法游标 450102、非法 size 400001
#   7. AC-6 未登录可搜
#   8. AC-7 超长关键词截断(不报错)
#
# 前置：服务以 `go run ./cmd/server 2>&1 | tee logs/dev.log` 启动，
#       docker 开发栈(cooking-mysql-dev / cooking-redis-dev)在跑。

set -e

BASE="http://localhost:8080/api/v1"
LOG_FILE="logs/dev.log"
# 用一个本步专属、几乎不可能和历史数据撞车的标题前缀，保证搜索断言可控
TAG="zztest$(date +%s)"

# 颜色
G='\033[0;32m'; R='\033[0;31m'; Y='\033[1;33m'; N='\033[0m'
ok()   { echo -e "${G}✓${N} $1"; }
fail() { echo -e "${R}✗${N} $1"; exit 1; }
info() { echo -e "${Y}→${N} $1"; }

# 前置检查
[ -f "$LOG_FILE" ] || fail "日志文件 $LOG_FILE 不存在，请用 'go run ./cmd/server 2>&1 | tee logs/dev.log' 启动服务"
command -v jq >/dev/null || fail "需要 jq 命令"

# ── 登录工具函数：给定手机号，返回 access_token（自动抓验证码） ──────────────
# 用法： TOKEN=$(login_as 13800138001)
login_as() {
  local phone="$1"
  curl -s -X POST "$BASE/auth/send-code" \
    -H 'Content-Type: application/json' \
    -d "{\"phone\":\"$phone\"}" >/dev/null
  sleep 0.3
  local code
  code=$(grep '\[SMS MOCK\]' "$LOG_FILE" | tail -1 | grep -oE '"code": "[0-9]+"' | grep -oE '[0-9]+')
  [ -n "$code" ] || { echo ""; return 1; }
  curl -s -X POST "$BASE/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"phone\":\"$phone\",\"code\":\"$code\"}"
}

# ── 1. R-1 回归：首次登录响应 created_at 非零值 ────────────────────────────
info "1. R-1 回归 —— 用一个全新手机号触发自动注册"
# 末 4 位用时间戳后 4 位，尽量保证是「新用户」走自动注册分支
NEW_PHONE="139${TAG: -8}"
NEW_PHONE="${NEW_PHONE:0:11}"
LOGIN_RESP=$(login_as "$NEW_PHONE")
CREATED_AT=$(echo "$LOGIN_RESP" | jq -r '.data.user.created_at')
TOKEN=$(echo "$LOGIN_RESP" | jq -r '.data.access_token')
USER_ID=$(echo "$LOGIN_RESP" | jq -r '.data.user.id')
[ "$TOKEN" != "null" ] && [ -n "$TOKEN" ] || fail "登录失败: $LOGIN_RESP"
# 零值会序列化成 -62135596800000；修复后应为一个近期的正毫秒时间戳
if [ "$CREATED_AT" = "null" ] || [ -z "$CREATED_AT" ]; then
  fail "created_at 缺失: $LOGIN_RESP"
fi
if [ "$CREATED_AT" -le 0 ] 2>/dev/null; then
  fail "R-1 未修复：created_at=$CREATED_AT（零值/负值）"
fi
# 进一步确认是「近期」时间（2025-01-01 之后的毫秒数 ~1735689600000）
if [ "$CREATED_AT" -lt 1735689600000 ] 2>/dev/null; then
  fail "R-1 可疑：created_at=$CREATED_AT 不像近期时间戳"
fi
ok "R-1 已修复，created_at=$CREATED_AT（user_id=$USER_ID）"

# ── 2. 造数据：发 3 篇标题可控的测试帖 ─────────────────────────────────────
info "2. 发 3 篇测试帖（标题含唯一前缀 $TAG）"
# 帖 A：标题里 TAG 出现 1 次，scene_tag=1
PA=$(curl -s -X POST "$BASE/posts" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"$TAG 红烧肉\",\"scene_tag\":1,\"content\":\"step7 a\"}")
PA_ID=$(echo "$PA" | jq -r '.data.post_id')
# 帖 B：scene_tag=2
PB=$(curl -s -X POST "$BASE/posts" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"$TAG 番茄炒蛋\",\"scene_tag\":2,\"content\":\"step7 b\"}")
PB_ID=$(echo "$PB" | jq -r '.data.post_id')
# 帖 C：scene_tag=1，标题不含 TAG —— 作为「不应被搜到」的反例
PC=$(curl -s -X POST "$BASE/posts" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"无关标题土豆丝\",\"scene_tag\":1,\"content\":\"step7 c\"}")
PC_ID=$(echo "$PC" | jq -r '.data.post_id')
for v in "$PA_ID" "$PB_ID" "$PC_ID"; do
  [ "$v" != "null" ] && [ -n "$v" ] || fail "发帖失败: A=$PA B=$PB C=$PC"
done
ok "3 篇测试帖已发布：A=$PA_ID(s1) B=$PB_ID(s2) C=$PC_ID(s1,无TAG)"

# ── 3. 关键词搜索命中 ──────────────────────────────────────────────────────
info "3. 搜索关键词 '$TAG' —— 应命中 A、B 两篇，不含 C"
S1=$(curl -s "$BASE/search?q=$TAG")
S1_CODE=$(echo "$S1" | jq -r '.code')
[ "$S1_CODE" = "0" ] || fail "搜索返回非 0: $S1"
S1_COUNT=$(echo "$S1" | jq -r '.data.posts | length')
[ "$S1_COUNT" = "2" ] || fail "期望命中 2 篇，实际 $S1_COUNT: $S1"
# C 不应出现
HAS_C=$(echo "$S1" | jq -r --arg c "$PC_ID" '.data.posts | map(.id == ($c|tonumber)) | any')
[ "$HAS_C" = "false" ] || fail "反例帖 C($PC_ID) 不应被搜到"
ok "命中 2 篇且不含反例 C"

# ── 4. scene_tag 服务端过滤 ───────────────────────────────────────────────
info "4. 搜索 '$TAG' + scene_tag=2 —— 应只剩 B"
S2=$(curl -s "$BASE/search?q=$TAG&scene_tag=2")
S2_COUNT=$(echo "$S2" | jq -r '.data.posts | length')
S2_ID=$(echo "$S2" | jq -r '.data.posts[0].id')
[ "$S2_COUNT" = "1" ] || fail "scene 过滤期望 1 篇，实际 $S2_COUNT: $S2"
[ "$S2_ID" = "$PB_ID" ] || fail "scene 过滤期望 B=$PB_ID，实际 $S2_ID"
ok "scene_tag 过滤正确，只剩 B=$PB_ID"

# ── 5. offset 游标翻页 ────────────────────────────────────────────────────
info "5-1. 搜索 '$TAG' size=1 —— 第一页 1 条 + has_more=true"
P1=$(curl -s "$BASE/search?q=$TAG&size=1")
P1_COUNT=$(echo "$P1" | jq -r '.data.posts | length')
P1_HASMORE=$(echo "$P1" | jq -r '.data.has_more')
P1_CURSOR=$(echo "$P1" | jq -r '.data.next_cursor')
[ "$P1_COUNT" = "1" ] || fail "第一页期望 1 条，实际 $P1_COUNT"
[ "$P1_HASMORE" = "true" ] || fail "第一页 has_more 期望 true，实际 $P1_HASMORE"
[ "$P1_CURSOR" = "1" ] || fail "第一页 next_cursor 期望 '1'(offset)，实际 '$P1_CURSOR'"
ok "第一页正确：1 条 has_more=true next_cursor=1"

info "5-2. 带 cursor=1 取第二页 —— 1 条 + has_more=false"
P2=$(curl -s "$BASE/search?q=$TAG&size=1&cursor=$P1_CURSOR")
P2_COUNT=$(echo "$P2" | jq -r '.data.posts | length')
P2_HASMORE=$(echo "$P2" | jq -r '.data.has_more')
P2_CURSOR=$(echo "$P2" | jq -r '.data.next_cursor')
[ "$P2_COUNT" = "1" ] || fail "第二页期望 1 条，实际 $P2_COUNT"
[ "$P2_HASMORE" = "false" ] || fail "第二页 has_more 期望 false，实际 $P2_HASMORE"
[ "$P2_CURSOR" = "" ] || fail "第二页 next_cursor 期望空串，实际 '$P2_CURSOR'"
ok "第二页正确：1 条 has_more=false next_cursor=空"

# ── 6. 错误用例 ───────────────────────────────────────────────────────────
info "6-1. 空关键词 → 450101"
E1=$(curl -s "$BASE/search?q=" | jq -r '.code')
[ "$E1" = "450101" ] || fail "空关键词期望 450101，实际 $E1"
ok "空关键词 450101 正确"

info "6-2. 纯空格关键词 → 450101"
E2=$(curl -s "$BASE/search?q=%20%20%20" | jq -r '.code')
[ "$E2" = "450101" ] || fail "纯空格期望 450101，实际 $E2"
ok "纯空格 450101 正确"

info "6-3. 非法游标 cursor=abc → 450102"
E3=$(curl -s "$BASE/search?q=$TAG&cursor=abc" | jq -r '.code')
[ "$E3" = "450102" ] || fail "非法游标期望 450102，实际 $E3"
ok "非法游标 450102 正确"

info "6-4. 非法 size=999 → 400001（binding 拦截）"
E4=$(curl -s "$BASE/search?q=$TAG&size=999" | jq -r '.code')
[ "$E4" = "400001" ] || fail "非法 size 期望 400001，实际 $E4"
ok "非法 size 400001 正确"

# ── 7. AC-6 未登录可搜 ────────────────────────────────────────────────────
info "7. AC-6 未登录用户可正常搜索（不带 Authorization 头）"
S_ANON=$(curl -s "$BASE/search?q=$TAG")
ANON_CODE=$(echo "$S_ANON" | jq -r '.code')
ANON_COUNT=$(echo "$S_ANON" | jq -r '.data.posts | length')
[ "$ANON_CODE" = "0" ] && [ "$ANON_COUNT" = "2" ] || fail "未登录搜索异常: $S_ANON"
ok "未登录搜索正常，命中 2 篇"

# ── 8. AC-7 超长关键词截断（不报错） ──────────────────────────────────────
info "8. AC-7 超长关键词(>50字符)应截断而非报错"
LONG=$(printf '%s' "$TAG"; printf 'x%.0s' {1..100})
E5=$(curl -s "$BASE/search?q=$LONG" | jq -r '.code')
# 截断后前缀仍是 $TAG，所以应当 code=0 正常返回（命中 A、B）
[ "$E5" = "0" ] || fail "超长关键词期望截断后正常(code=0)，实际 $E5"
ok "超长关键词被截断，正常返回"

echo ""
echo -e "${G}========================================${N}"
echo -e "${G}  Step 7 端到端验证全部通过 ✓${N}"
echo -e "${G}  搜索模块 8 大类断言 + R-1 回归 通过${N}"
echo -e "${G}========================================${N}"
echo ""
echo "提示：本次造的测试帖标题前缀为 $TAG，如需清理："
echo "  docker exec cooking-mysql-dev mysql -uroot -pcooking123 cooking_platform \\"
echo "    -e \"DELETE FROM posts WHERE title LIKE '${TAG}%' OR title='无关标题土豆丝';\""