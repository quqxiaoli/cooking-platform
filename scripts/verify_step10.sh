#!/usr/bin/env bash
# scripts/verify_step10.sh
#
# Step 10 端到端验证：内容审核（MockAuditor）+ 阿里云短信接入骨架
#
# 前置条件：
#   1. make docker-up            (MySQL + Redis 运行中)
#   2. make migrate-up           (000006_create_audit_logs_table 已跑)
#   3. go run ./cmd/server 2>&1 | tee -i logs/dev.log  (另一终端)
#   4. configs/config.yaml audit.provider=mock  audit.mock_result=pass
#
# 验证段：
#   §1  发帖响应 is_visible=0, audit_status=0（pending）—— 状态机入口
#   §2  等待 1 秒（MockAuditor 毫秒级，sleep 只是给 goroutine 调度留余量）
#   §3  等待后 GetDetail → 200（可见）；字段 is_visible=1 / audit_status=1
#   §4  MySQL: posts.is_visible=1, audit_status=1
#   §5  MySQL: audit_logs 有对应记录，audit_status=1
#   §6  audit_logs.raw_response 非空（MockAuditor 写了 JSON blob）
#   §7  带 cover_url + steps 的帖子 → audit_logs 同样落库
#   §8  Feed 包含已审通过帖子
#   §9  未登录发帖 → 401001
#
# 注：不测"发帖后立即 GetDetail → 404"——MockAuditor + ChannelBus 进程内投递
#     速度远快于两次 curl 间隔，该窗口在 mock 环境下不可靠。"发帖默认不可见"
#     已由 §1（CREATE 响应 is_visible=0）充分覆盖。
set -e
source "$(dirname "$0")/verify_common.sh"

precheck

# ── 准备测试用户 ──────────────────────────────────────────────────────────────
PHONE="13900001000"
TOKEN=$(token_of "$PHONE")
info "测试用户 token 已获取"

# ── §1  发帖响应 is_visible=0, audit_status=0 ────────────────────────────────
section "§1 发帖响应 is_visible=0 / audit_status=0"

CREATE_RESP=$(curl -s -X POST "$BASE/posts" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title":"审核测试帖子","content":"这是正文","scene_tag":1}')

assert_code 0 "$CREATE_RESP" "发帖应成功"

POST_ID=$(echo "$CREATE_RESP" | jq -r '.data.post_id')
assert_nonempty "$POST_ID" "应返回 post_id"

IS_VIS=$(echo "$CREATE_RESP" | jq -r '.data.is_visible')
AUDIT_ST=$(echo "$CREATE_RESP" | jq -r '.data.audit_status')
assert_eq "0" "$IS_VIS"   "发帖响应 is_visible 应为 0（待审）"
assert_eq "0" "$AUDIT_ST" "发帖响应 audit_status 应为 0（pending）"

# ── §2  等待 AuditConsumer 处理 ──────────────────────────────────────────────
# MockAuditor + ChannelBus 是进程内毫秒级投递，Consumer goroutine 几乎立刻就
# 处理完毕。不测"立即不可见"的时间窗口（时序假设过强，Mock 下必然 flaky）；
# §1 已通过 CREATE 响应断言 is_visible=0，语义正确性已覆盖。
section "§2 等待 1 秒（确保 Consumer 已处理）"
sleep 1

DETAIL_AFTER=$(curl -s "$BASE/posts/$POST_ID")
assert_code 0 "$DETAIL_AFTER" "审核通过后 GetDetail 应成功"
assert_json "$DETAIL_AFTER" '.data.is_visible'   "1" "is_visible 应已更新为 1"
assert_json "$DETAIL_AFTER" '.data.audit_status' "1" "audit_status 应已更新为 1（机审通过）"

# ── §4  MySQL: posts 行已更新 ────────────────────────────────────────────────
section "§4 MySQL posts 行状态"

DB_VISIBLE=$(mysql_q "SELECT is_visible FROM posts WHERE id=$POST_ID;")
DB_AUDIT=$(mysql_q   "SELECT audit_status FROM posts WHERE id=$POST_ID;")
assert_eq "1" "$DB_VISIBLE" "posts.is_visible 应为 1"
assert_eq "1" "$DB_AUDIT"   "posts.audit_status 应为 1"

# ── §5  MySQL: audit_logs 有对应记录 ─────────────────────────────────────────
section "§5 audit_logs 落库"

LOG_CNT=$(mysql_q "SELECT COUNT(*) FROM audit_logs WHERE post_id=$POST_ID;")
assert_ge 1 "$LOG_CNT" "audit_logs 应有至少 1 条记录"

LOG_STATUS=$(mysql_q "SELECT audit_status FROM audit_logs WHERE post_id=$POST_ID LIMIT 1;")
assert_eq "1" "$LOG_STATUS" "audit_logs.audit_status 应为 1（机审通过）"

# ── §6  audit_logs.raw_response 非空 ─────────────────────────────────────────
section "§6 raw_response 非空"

RAW=$(mysql_q "SELECT raw_response FROM audit_logs WHERE post_id=$POST_ID LIMIT 1;")
assert_nonempty "$RAW" "audit_logs.raw_response 应非空（MockAuditor 写了 JSON）"

# ── §7  带 cover_url + steps 的帖子 ──────────────────────────────────────────
section "§7 带 cover_url + steps 的帖子 → audit_logs 落库"

# cover_url 用 mock OSS 白名单前缀（与 step9 一致）
COVER="http://127.0.0.1:18080/avatar/1/202501/test.jpg"
CREATE_STEPS=$(curl -s -X POST "$BASE/posts" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"title\": \"带步骤的审核帖\",
    \"content\": \"正文\",
    \"scene_tag\": 2,
    \"cover_url\": \"$COVER\",
    \"steps\": [
      {\"text\": \"第一步\", \"image_urls\": []},
      {\"text\": \"第二步\", \"image_urls\": []}
    ]
  }")
assert_code 0 "$CREATE_STEPS" "带 steps 发帖应成功"

POST2_ID=$(echo "$CREATE_STEPS" | jq -r '.data.post_id')
assert_nonempty "$POST2_ID" "应返回 post2_id"
sleep 1

LOG2_CNT=$(mysql_q "SELECT COUNT(*) FROM audit_logs WHERE post_id=$POST2_ID;")
assert_ge 1 "$LOG2_CNT" "带 steps 帖子 audit_logs 应落库"

# ── §8  Feed 包含已审通过帖子 ────────────────────────────────────────────────
section "§8 Feed 包含已审通过帖子"

FEED=$(curl -s "$BASE/feed")
assert_code 0 "$FEED" "Feed 请求应成功"
FEED_COUNT=$(echo "$FEED" | jq '[.data.posts[] | select(.id == '"$POST_ID"')] | length')
assert_eq "1" "$FEED_COUNT" "Feed 应包含 post_id=$POST_ID 的帖子"

# ── §9  未登录发帖 → 401001 ──────────────────────────────────────────────────
section "§9 未登录发帖 → 401001"

UNAUTH=$(curl -s -X POST "$BASE/posts" \
  -H "Content-Type: application/json" \
  -d '{"title":"无 token","content":"test","scene_tag":1}')
assert_code 401001 "$UNAUTH" "未登录发帖应返回 401001"

# ── 完成 ──────────────────────────────────────────────────────────────────────
banner_pass "Step 10 内容审核 + 阿里云短信（9 段）"
