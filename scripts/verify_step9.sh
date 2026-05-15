#!/usr/bin/env bash
# scripts/verify_step9.sh
#
# Step 9 · 图片上传模块端到端验证。
# 覆盖路径：
#   1. presign 接口结构 + 必备字段
#   2. 真实 PUT 到 mock OSS listener（决策 2c 已确认）
#   3. callback 成功 + 返回 url == public_url
#   4. callback 重放（同一 nonce 二次提交）应失败 460104
#   5. callback 跨用户消费别人 nonce 应失败 460104
#   6. content_type 不在白名单（DTO binding 400001）
#   7. UpdateProfile 接受白名单 OSS URL
#   8. UpdateProfile 拒绝非白名单 URL（460105）
#   9. CreatePost with steps & image_urls 成功 + GetDetail 看到 steps
#  10. CreatePost cover_url 非白名单（460105）
#  11. 未登录调 presign（401001）
#
# 前置条件：
#   - cooking-mysql-dev / cooking-redis-dev 容器在跑（make docker-up）
#   - 服务以 `go run ./cmd/server 2>&1 | tee -i logs/dev.log` 启动
#   - migrations 已应用到 000005

set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/verify_common.sh
source "$SCRIPT_DIR/verify_common.sh"

# Mock OSS 监听地址 + URL 前缀（必须与 configs/config.yaml 一致）
MOCK_OSS_BASE="${MOCK_OSS_BASE:-http://127.0.0.1:18080}"
OSS_PREFIX="${OSS_PREFIX:-http://127.0.0.1:18080/}"

precheck

# ── 段落 1 · presign 基本流程 ─────────────────────────────────────────────────
section "1. presign 接口"

# 用两个独立手机号，分别拿 token；同时记下 user_id 供后续断言。
PHONE_A="13800009001"
PHONE_B="13800009002"

RESP_A=$(login_resp "$PHONE_A")
TOKEN_A=$(echo "$RESP_A" | jq -r '.data.access_token')
UID_A=$(echo "$RESP_A"   | jq -r '.data.user.id')
assert_nonempty "$TOKEN_A" "用户 A 拿到 access_token"
assert_nonempty "$UID_A"   "用户 A 拿到 user_id"

RESP_B=$(login_resp "$PHONE_B")
TOKEN_B=$(echo "$RESP_B" | jq -r '.data.access_token')
UID_B=$(echo "$RESP_B"   | jq -r '.data.user.id')
assert_nonempty "$TOKEN_B" "用户 B 拿到 access_token"

# presign avatar — 应返回完整的预签名包
PRESIGN_REQ='{"filename":"avatar.png","content_type":"image/png","size":2048,"purpose":"avatar"}'
PRESIGN_RESP=$(curl -s -X POST "$BASE/upload/presign" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "$PRESIGN_REQ")

assert_code 0 "$PRESIGN_RESP" "用户 A presign avatar 成功"
UPLOAD_URL=$(echo "$PRESIGN_RESP" | jq -r '.data.upload_url')
PUBLIC_URL=$(echo "$PRESIGN_RESP" | jq -r '.data.public_url')
NONCE=$(echo "$PRESIGN_RESP"     | jq -r '.data.nonce')
METHOD=$(echo "$PRESIGN_RESP"    | jq -r '.data.method')
EXPIRES_AT=$(echo "$PRESIGN_RESP" | jq -r '.data.expires_at')
CT_HEADER=$(echo "$PRESIGN_RESP" | jq -r '.data.headers["Content-Type"]')

assert_nonempty "$UPLOAD_URL"  "返回 upload_url"
assert_nonempty "$PUBLIC_URL"  "返回 public_url"
assert_nonempty "$NONCE"       "返回 nonce"
assert_eq "PUT"        "$METHOD"     "method == PUT"
assert_eq "image/png"  "$CT_HEADER"  "headers.Content-Type == image/png"
assert_ge 1 "$EXPIRES_AT" "expires_at 为正数（UnixMilli）"

# public_url 必须以 OSS_PREFIX 开头（白名单基线）
case "$PUBLIC_URL" in
  "$OSS_PREFIX"*) ok "public_url 落在白名单前缀内";;
  *) fail "public_url 不在白名单前缀: $PUBLIC_URL";;
esac

# Redis 应该能看到 upload:nonce:{NONCE}（值是 JSON）
RAW_NONCE=$(redis_q GET "upload:nonce:$NONCE" || true)
assert_nonempty "$RAW_NONCE" "Redis 中 upload:nonce:{nonce} 已落地"

# ── 段落 2 · 真实 PUT 到 mock OSS ─────────────────────────────────────────────
section "2. 真实 PUT 到 mock OSS"

# 构造 2KB 假二进制内容
TMP_BIN="$(mktemp -t step9_payload.XXXXXX)"
dd if=/dev/urandom of="$TMP_BIN" bs=1024 count=2 status=none

HTTP_STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
  -X PUT "$UPLOAD_URL" \
  -H "Content-Type: image/png" \
  --data-binary @"$TMP_BIN")
assert_eq "200" "$HTTP_STATUS" "PUT $UPLOAD_URL 返回 200"

rm -f "$TMP_BIN"

# ── 段落 3 · callback 成功路径 ────────────────────────────────────────────────
section "3. callback 成功路径"

CB_RESP=$(curl -s -X POST "$BASE/upload/callback" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"nonce\":\"$NONCE\"}")
assert_code 0 "$CB_RESP" "callback 首次成功"

CB_URL=$(echo "$CB_RESP" | jq -r '.data.url')
assert_eq "$PUBLIC_URL" "$CB_URL" "callback 返回 url == presign public_url"

# Redis 中 nonce 应已被 GETDEL 消费
RAW_AFTER=$(redis_q GET "upload:nonce:$NONCE" || true)
assert_eq "" "$RAW_AFTER" "callback 消费后 Redis 中 nonce 已不存在"

# ── 段落 4 · callback 重放 ────────────────────────────────────────────────────
section "4. callback 重放（同 nonce 再次提交）"

CB_REPLAY=$(curl -s -X POST "$BASE/upload/callback" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"nonce\":\"$NONCE\"}")
assert_code 460104 "$CB_REPLAY" "重放 nonce 应返回 460104"

# ── 段落 5 · 跨用户消费别人 nonce ─────────────────────────────────────────────
section "5. 跨用户消费别人 nonce"

# 用户 A 先 presign 拿到新 nonce
PRESIGN_X=$(curl -s -X POST "$BASE/upload/presign" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "$PRESIGN_REQ")
assert_code 0 "$PRESIGN_X" "用户 A 第二次 presign 成功"
NONCE_X=$(echo "$PRESIGN_X" | jq -r '.data.nonce')

# 用户 B 用 A 的 nonce 调 callback → 460104
CB_CROSS=$(curl -s -X POST "$BASE/upload/callback" \
  -H "Authorization: Bearer $TOKEN_B" \
  -H 'Content-Type: application/json' \
  -d "{\"nonce\":\"$NONCE_X\"}")
assert_code 460104 "$CB_CROSS" "用户 B 消费 A 的 nonce 应失败 460104"

# 该 nonce 已被 GETDEL 删除（无论成败 GETDEL 都会删）
RAW_X=$(redis_q GET "upload:nonce:$NONCE_X" || true)
assert_eq "" "$RAW_X" "跨用户 callback 失败后 Redis 中 nonce 已删除（GETDEL 是原子）"

# ── 段落 6 · DTO binding 拒绝非白名单 content_type ─────────────────────────────
section "6. DTO binding 拒绝非白名单 content_type"

BAD_CT='{"filename":"x.gif","content_type":"image/gif","size":1024,"purpose":"step"}'
BAD_CT_RESP=$(curl -s -X POST "$BASE/upload/presign" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "$BAD_CT")
assert_code 400001 "$BAD_CT_RESP" "content_type=image/gif 应被 DTO 拒绝（400001）"

# ── 段落 7-8 · UpdateProfile 白名单 ──────────────────────────────────────────
section "7. UpdateProfile 接受白名单 OSS URL"

# 用户 A 用之前 callback 返回的 url 更新头像
PROFILE_OK=$(curl -s -X PATCH "$BASE/users/me" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"avatar_url\":\"$PUBLIC_URL\"}")
assert_code 0 "$PROFILE_OK" "更新头像为白名单 URL 成功"

# 拉一次个人资料确认落库
ME_RESP=$(curl -s "$BASE/users/me" -H "Authorization: Bearer $TOKEN_A")
ME_AVATAR=$(echo "$ME_RESP" | jq -r '.data.avatar_url')
assert_eq "$PUBLIC_URL" "$ME_AVATAR" "/users/me 读取到新头像"

section "8. UpdateProfile 拒绝非白名单 URL"
PROFILE_BAD=$(curl -s -X PATCH "$BASE/users/me" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d '{"avatar_url":"https://evil.example.com/x.png"}')
assert_code 460105 "$PROFILE_BAD" "外部域名头像被拒（460105）"

# ── 段落 9 · 发帖 with steps & image_urls ────────────────────────────────────
section "9. 发帖 with steps & image_urls"

# 先为 cover_url 拿一个白名单 URL
COVER_PRESIGN=$(curl -s -X POST "$BASE/upload/presign" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d '{"filename":"cover.jpg","content_type":"image/jpeg","size":4096,"purpose":"cover"}')
assert_code 0 "$COVER_PRESIGN" "cover 预签名成功"
COVER_URL=$(echo "$COVER_PRESIGN" | jq -r '.data.public_url')
COVER_UPLOAD=$(echo "$COVER_PRESIGN" | jq -r '.data.upload_url')
COVER_NONCE=$(echo "$COVER_PRESIGN" | jq -r '.data.nonce')

# 走完整 PUT + callback，确保 cover_url 落到一个真实"上传过"的 object
TMP_BIN="$(mktemp -t step9_cover.XXXXXX)"
dd if=/dev/urandom of="$TMP_BIN" bs=1024 count=4 status=none
curl -s -o /dev/null -X PUT "$COVER_UPLOAD" \
  -H "Content-Type: image/jpeg" --data-binary @"$TMP_BIN"
rm -f "$TMP_BIN"
curl -s -X POST "$BASE/upload/callback" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "{\"nonce\":\"$COVER_NONCE\"}" > /dev/null

# 同样为两张 step 图片各拿一个白名单 URL（简化：只走 presign 不 PUT，service
# 层只校验 URL 是否白名单，不验证 OSS 上文件是否真存在）
STEP1_RESP=$(curl -s -X POST "$BASE/upload/presign" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d '{"filename":"s1.jpg","content_type":"image/jpeg","size":2048,"purpose":"step"}')
STEP1_URL=$(echo "$STEP1_RESP" | jq -r '.data.public_url')

STEP2_RESP=$(curl -s -X POST "$BASE/upload/presign" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d '{"filename":"s2.jpg","content_type":"image/jpeg","size":2048,"purpose":"step"}')
STEP2_URL=$(echo "$STEP2_RESP" | jq -r '.data.public_url')

POST_BODY=$(jq -n \
  --arg cover "$COVER_URL" \
  --arg s1    "$STEP1_URL" \
  --arg s2    "$STEP2_URL" \
  '{
     title:     "step9-end2end-with-steps",
     scene_tag: 5,
     content:   "正文 fallback",
     cover_url: $cover,
     steps: [
       { text: "第一步：备料",     image_urls: [$s1] },
       { text: "第二步：开火翻炒", image_urls: [$s2] }
     ]
   }')
POST_RESP=$(curl -s -X POST "$BASE/posts" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d "$POST_BODY")
assert_code 0 "$POST_RESP" "发帖 with steps 成功"
POST_ID=$(echo "$POST_RESP" | jq -r '.data.post_id')
assert_nonempty "$POST_ID" "拿到 post_id"

# DB 中 post_steps 应有 2 行
STEP_COUNT=$(mysql_q "SELECT COUNT(*) FROM post_steps WHERE post_id=$POST_ID")
assert_eq "2" "$STEP_COUNT" "post_steps 表中有 2 条记录"

# GetDetail 看 steps
DETAIL=$(curl -s "$BASE/posts/$POST_ID")
assert_code 0 "$DETAIL" "GetDetail 成功"
assert_json "$DETAIL" '.data.steps | length' '2' "detail.steps 长度 2"
assert_json "$DETAIL" '.data.steps[0].step_no' '1' "step_no 从 1 开始"
assert_json "$DETAIL" '.data.steps[0].text' '第一步：备料' "step1.text 正确"
assert_json "$DETAIL" '.data.steps[0].image_urls | length' '1' "step1 image_urls 一张"
assert_json "$DETAIL" '.data.steps[1].image_urls[0]' "$STEP2_URL" "step2 image_urls[0] 等于上传 URL"
assert_eq "$COVER_URL" "$(echo "$DETAIL" | jq -r '.data.cover_url')" "detail.cover_url 等于上传 URL"

# ── 段落 10 · cover_url 非白名单 ──────────────────────────────────────────────
section "10. cover_url 非白名单应被 service 拒绝"

POST_BAD_COVER=$(curl -s -X POST "$BASE/posts" \
  -H "Authorization: Bearer $TOKEN_A" \
  -H 'Content-Type: application/json' \
  -d '{"title":"bad-cover","scene_tag":1,"content":"","cover_url":"https://evil.example.com/x.jpg"}')
assert_code 460105 "$POST_BAD_COVER" "外部域名 cover_url 被拒（460105）"

# ── 段落 11 · 未登录调 presign ────────────────────────────────────────────────
section "11. 未登录调 presign"

NOAUTH=$(curl -s -X POST "$BASE/upload/presign" \
  -H 'Content-Type: application/json' \
  -d "$PRESIGN_REQ")
assert_code 401001 "$NOAUTH" "未登录返回 401001"

banner_pass "Step 9 图片上传模块"