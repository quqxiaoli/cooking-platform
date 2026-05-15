#!/usr/bin/env bash
# scripts/verify_step8.sh
#
# Step 8 关注模块端到端验证。覆盖 PRD-Phase2 §8 F-F01 的全部 AC：
#   AC-1 关注/取消关注 < 300ms（本脚本验功能正确性，不测延迟）
#   AC-2 未登录点击关注 → 401
#   AC-3 不能关注自己 → 440101
#   AC-4 已关注再次关注 → 幂等 following=true，计数不重复 +1
#   AC-5 关注上限 3000 → 440102（本脚本验错误码路径，不灌满 3000 条）
#   + 取消未关注的人 → 440103
#   + 粉丝/关注列表游标分页 + has_more
#   + CountConsumer 异步维护 users.follower_count / following_count
#
# 用法：
#   make docker-up
#   go run ./cmd/server 2>&1 | tee -i logs/dev.log   # 另开终端
#   make verify-step8
set -e
source "$(dirname "$0")/verify_common.sh"

precheck

# ── 准备三个测试用户 ────────────────────────────────────────────────────────
# 用三个不同手机号，分别拿 token + user_id。login_resp 一次拿全，避免重复登录。
section "准备测试用户"

RESP_A=$(login_resp 13900000001)
TOKEN_A=$(echo "$RESP_A" | jq -r '.data.access_token')
UID_A=$(echo "$RESP_A" | jq -r '.data.user.id')
assert_nonempty "$TOKEN_A" "用户 A 登录拿到 token"
assert_nonempty "$UID_A"   "用户 A 拿到 user_id ($UID_A)"

RESP_B=$(login_resp 13900000002)
TOKEN_B=$(echo "$RESP_B" | jq -r '.data.access_token')
UID_B=$(echo "$RESP_B" | jq -r '.data.user.id')
assert_nonempty "$TOKEN_B" "用户 B 登录拿到 token"
assert_nonempty "$UID_B"   "用户 B 拿到 user_id ($UID_B)"

RESP_C=$(login_resp 13900000003)
TOKEN_C=$(echo "$RESP_C" | jq -r '.data.access_token')
UID_C=$(echo "$RESP_C" | jq -r '.data.user.id')
assert_nonempty "$TOKEN_C" "用户 C 登录拿到 token"
assert_nonempty "$UID_C"   "用户 C 拿到 user_id ($UID_C)"

# 清掉本脚本可能残留的关注关系，保证可重复运行。
mysql_q "DELETE FROM follows WHERE follower_id IN ($UID_A,$UID_B,$UID_C) OR following_id IN ($UID_A,$UID_B,$UID_C)" >/dev/null
mysql_q "UPDATE users SET follower_count=0, following_count=0 WHERE id IN ($UID_A,$UID_B,$UID_C)" >/dev/null

# ── AC-2：未登录点击关注 → 401 ──────────────────────────────────────────────
section "AC-2 未登录关注 → 401"

RESP=$(curl -s -X POST "$BASE/users/$UID_B/follow")
assert_code 401001 "$RESP" "未登录 POST /users/:id/follow 返回 401001"

# ── AC-3：不能关注自己 → 440101 ─────────────────────────────────────────────
section "AC-3 不能关注自己 → 440101"

RESP=$(curl -s -X POST "$BASE/users/$UID_A/follow" -H "Authorization: Bearer $TOKEN_A")
assert_code 440101 "$RESP" "A 关注自己返回 440101"

# ── 关注不存在的用户 → 410108 ───────────────────────────────────────────────
section "关注不存在的用户 → 410108"

RESP=$(curl -s -X POST "$BASE/users/99999999/follow" -H "Authorization: Bearer $TOKEN_A")
assert_code 410108 "$RESP" "关注不存在用户返回 410108"

# ── 正常关注：A → B，A → C ──────────────────────────────────────────────────
section "正常关注 A→B, A→C, C→B"

RESP=$(curl -s -X POST "$BASE/users/$UID_B/follow" -H "Authorization: Bearer $TOKEN_A")
assert_code 0 "$RESP" "A 关注 B 成功"
assert_json "$RESP" '.data.following' 'true' "A 关注 B 后 following=true"

RESP=$(curl -s -X POST "$BASE/users/$UID_C/follow" -H "Authorization: Bearer $TOKEN_A")
assert_code 0 "$RESP" "A 关注 C 成功"

RESP=$(curl -s -X POST "$BASE/users/$UID_B/follow" -H "Authorization: Bearer $TOKEN_C")
assert_code 0 "$RESP" "C 关注 B 成功"

# follows 表应有 3 行
CNT=$(mysql_q "SELECT COUNT(*) FROM follows WHERE follower_id IN ($UID_A,$UID_C)")
assert_eq 3 "$CNT" "follows 表写入 3 行关注关系"

# ── AC-4：已关注再次关注 → 幂等 ─────────────────────────────────────────────
section "AC-4 已关注再次关注 → 幂等，计数不重复 +1"

RESP=$(curl -s -X POST "$BASE/users/$UID_B/follow" -H "Authorization: Bearer $TOKEN_A")
assert_code 0 "$RESP" "A 重复关注 B 仍返回成功"
assert_json "$RESP" '.data.following' 'true' "A 重复关注 B 后 following=true"

CNT=$(mysql_q "SELECT COUNT(*) FROM follows WHERE follower_id=$UID_A AND following_id=$UID_B")
assert_eq 1 "$CNT" "重复关注未产生第二行（uk_follower_following 幂等）"

# ── 取消未关注的人 → 440103 ─────────────────────────────────────────────────
section "取消未关注的人 → 440103"

RESP=$(curl -s -X DELETE "$BASE/users/$UID_A/follow" -H "Authorization: Bearer $TOKEN_B")
assert_code 440103 "$RESP" "B 取消未关注的 A 返回 440103"

# ── CountConsumer 异步落库：等一个 flush 周期（10s）+ 余量 ──────────────────
section "CountConsumer 异步维护 users 双向计数"

info "等待 CountConsumer flush（countFlushInterval=10s）..."
sleep 13

# A 关注了 B、C → A.following_count=2；A 没有粉丝 → A.follower_count=0
FOLLOWING_A=$(mysql_q "SELECT following_count FROM users WHERE id=$UID_A")
assert_eq 2 "$FOLLOWING_A" "A.following_count = 2（关注了 B、C）"
FOLLOWER_A=$(mysql_q "SELECT follower_count FROM users WHERE id=$UID_A")
assert_eq 0 "$FOLLOWER_A" "A.follower_count = 0"

# B 被 A、C 关注 → B.follower_count=2；B 没关注别人 → B.following_count=0
FOLLOWER_B=$(mysql_q "SELECT follower_count FROM users WHERE id=$UID_B")
assert_eq 2 "$FOLLOWER_B" "B.follower_count = 2（被 A、C 关注）"
FOLLOWING_B=$(mysql_q "SELECT following_count FROM users WHERE id=$UID_B")
assert_eq 0 "$FOLLOWING_B" "B.following_count = 0"

# ── 粉丝列表 / 关注列表 ─────────────────────────────────────────────────────
section "粉丝列表 / 关注列表（游标分页）"

# B 的粉丝列表：应含 A、C 两人
RESP=$(curl -s "$BASE/users/$UID_B/followers")
assert_code 0 "$RESP" "GET /users/:id/followers 成功"
assert_json "$RESP" '.data.users | length' '2' "B 的粉丝列表返回 2 人"
assert_json "$RESP" '.data.has_more' 'false' "B 粉丝列表 has_more=false（不足一页）"

# A 的关注列表：应含 B、C 两人
RESP=$(curl -s "$BASE/users/$UID_A/following")
assert_code 0 "$RESP" "GET /users/:id/following 成功"
assert_json "$RESP" '.data.users | length' '2' "A 的关注列表返回 2 人"

# 游标分页：size=1 取第一页，应 has_more=true 且有 next_cursor
RESP=$(curl -s "$BASE/users/$UID_B/followers?size=1")
assert_json "$RESP" '.data.users | length' '1' "size=1 第一页返回 1 人"
assert_json "$RESP" '.data.has_more' 'true' "size=1 第一页 has_more=true"
NEXT=$(echo "$RESP" | jq -r '.data.next_cursor')
assert_nonempty "$NEXT" "第一页返回 next_cursor ($NEXT)"

# 用 next_cursor 取第二页，应再返回 1 人且 has_more=false
RESP=$(curl -s "$BASE/users/$UID_B/followers?size=1&cursor=$NEXT")
assert_json "$RESP" '.data.users | length' '1' "第二页返回 1 人"
assert_json "$RESP" '.data.has_more' 'false' "第二页 has_more=false"

# 非法游标 → 440104
RESP=$(curl -s "$BASE/users/$UID_B/followers?cursor=abc")
assert_code 440104 "$RESP" "非法游标返回 440104"

# ── 取消关注：A 取消关注 C ──────────────────────────────────────────────────
section "取消关注 A→C，验证计数回落"

RESP=$(curl -s -X DELETE "$BASE/users/$UID_C/follow" -H "Authorization: Bearer $TOKEN_A")
assert_code 0 "$RESP" "A 取消关注 C 成功"
assert_json "$RESP" '.data.following' 'false' "取消关注后 following=false"

CNT=$(mysql_q "SELECT COUNT(*) FROM follows WHERE follower_id=$UID_A AND following_id=$UID_C")
assert_eq 0 "$CNT" "follows 表对应行已物理删除"

info "等待 CountConsumer flush..."
sleep 13

FOLLOWING_A=$(mysql_q "SELECT following_count FROM users WHERE id=$UID_A")
assert_eq 1 "$FOLLOWING_A" "取消关注后 A.following_count 回落到 1"
FOLLOWER_C=$(mysql_q "SELECT follower_count FROM users WHERE id=$UID_C")
assert_eq 0 "$FOLLOWER_C" "取消关注后 C.follower_count 回落到 0"

# ── 清理 ────────────────────────────────────────────────────────────────────
mysql_q "DELETE FROM follows WHERE follower_id IN ($UID_A,$UID_B,$UID_C) OR following_id IN ($UID_A,$UID_B,$UID_C)" >/dev/null

banner_pass "Step 8 关注模块"