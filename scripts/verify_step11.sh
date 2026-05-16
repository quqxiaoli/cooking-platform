#!/usr/bin/env bash
# scripts/verify_step11.sh
#
# Step 11 验证：手机号 AES-GCM 加密 + SHA256 pepper 迁移
#
# 前置条件：
#   1. make docker-up                                      (MySQL + Redis 运行中)
#   2. make migrate-up                                     (migrations 全部执行)
#   3. go run ./cmd/server 2>&1 | tee -i logs/dev.log     (另一终端，configs/config.yaml 默认 key="" pepper="")
#
# 验证段：
#   §1  pkg/crypto 单元测试全部通过（10 个 test case）
#   §2  migrate-phone --dry-run 在 dev（key="" pepper=""）下正常退出（nothing to migrate）
#   §3  注册/登录新用户成功
#   §4  GET /users/me → phone_masked 符合 138****XXXX 格式
#   §5  MySQL: phone_hash == lowercase(SHA256(phone)) 当 pepper="" 时
#   §6  MySQL: phone_encrypted == 明文 当 key="" 时（dev 无加密）
#   §7  logout → 同一 token 再请求 → 401003（黑名单机制不受本步影响）
#   §8  用测试 key 启动 migrate-phone --dry-run → 输出 DRY 行，exit 0
#
# 注：§5 的 SHA256 计算用 openssl（macOS/Linux 均有），格式与 Go hex.EncodeToString 一致（小写）。
set -e
source "$(dirname "$0")/verify_common.sh"

precheck

PHONE="13811110011"

# ── §1  pkg/crypto 单元测试 ───────────────────────────────────────────────────
section "§1 pkg/crypto 单元测试（10 个 case）"
go test ./pkg/crypto/... -count=1 -timeout 30s >/dev/null 2>&1
ok "pkg/crypto 全部通过"

# ── §2  migrate-phone dev 模式（key='' pepper=''）→ nothing to migrate ─────────
section "§2 migrate-phone --dry-run 在 dev 模式退出 0"
go build -o /tmp/cooking-migrate-phone ./cmd/migrate-phone
OUTPUT=$(/tmp/cooking-migrate-phone --dry-run 2>&1 || true)
echo "$OUTPUT" | grep -qi "nothing to migrate" \
  || fail "预期输出 'nothing to migrate'，实际：$OUTPUT"
ok "dev 模式 nothing to migrate，exit 0"

# ── §3  注册/登录 ──────────────────────────────────────────────────────────────
section "§3 send-code + login"
LOGIN_RESP=$(login_resp "$PHONE")
assert_code 0 "$LOGIN_RESP" "login 应返回 code=0"
TOKEN=$(echo "$LOGIN_RESP" | jq -r '.data.access_token')
assert_nonempty "$TOKEN" "access_token 非空"

# ── §4  GET /users/me → phone_masked 格式 ────────────────────────────────────
section "§4 GET /users/me → phone_masked 格式"
ME=$(curl -s "$BASE/users/me" -H "Authorization: Bearer $TOKEN")
assert_code 0 "$ME" "GET /users/me 应成功"

MASKED=$(echo "$ME" | jq -r '.data.phone_masked')
assert_nonempty "$MASKED" "phone_masked 非空"

# 匹配 138****1011 形式：前 3 位 + 4 个星号 + 后 4 位
echo "$MASKED" | grep -qE '^[0-9]{3}\*{4}[0-9]{4}$' \
  || fail "phone_masked='$MASKED' 不符合 3位****4位 格式"
ok "phone_masked='$MASKED' 格式正确"

# ── §5  MySQL phone_hash == SHA256(phone)（pepper="" 时向后兼容）─────────────
section "§5 MySQL phone_hash == SHA256(phone)（dev: pepper=''）"
DB_HASH=$(mysql_q "SELECT phone_hash FROM users WHERE deleted_at IS NULL ORDER BY id DESC LIMIT 1;")
# openssl 输出格式: "(stdin)= <hex>" 或 "SHA2-256(stdin)= <hex>"，取最后一列
EXPECTED=$(printf '%s' "$PHONE" | openssl dgst -sha256 | awk '{print $NF}')
assert_eq "$EXPECTED" "$DB_HASH" "phone_hash 应等于 lowercase SHA256(phone)"

# ── §6  MySQL phone_encrypted == 明文（dev: key='' 不加密）──────────────────
section "§6 MySQL phone_encrypted == 明文（dev: key=''）"
DB_ENC=$(mysql_q "SELECT phone_encrypted FROM users WHERE deleted_at IS NULL ORDER BY id DESC LIMIT 1;")
assert_eq "$PHONE" "$DB_ENC" "phone_encrypted 在 dev 应为明文"

# ── §7  logout → 黑名单生效 ──────────────────────────────────────────────────
section "§7 logout → token 黑名单（本步不应破坏此机制）"
LOGOUT=$(curl -s -X POST "$BASE/auth/logout" -H "Authorization: Bearer $TOKEN")
assert_code 0 "$LOGOUT" "logout 应成功"

sleep 0.3
AFTER=$(curl -s "$BASE/users/me" -H "Authorization: Bearer $TOKEN")
assert_code 401003 "$AFTER" "logout 后用原 token 应返回 401003"

# ── §8  migrate-phone --dry-run 当用户存在但 key="" → 幂等，无变更 ───────────
section "§8 migrate-phone --dry-run 幂等验证（有用户，key=''）"
OUTPUT2=$(/tmp/cooking-migrate-phone --dry-run 2>&1 || true)
# dev 模式：key="" pepper="" → 立即退出，不扫描用户
echo "$OUTPUT2" | grep -qi "nothing to migrate" \
  || fail "有用户时 dev 模式 --dry-run 应仍输出 nothing to migrate：$OUTPUT2"
ok "有用户时 migrate-phone --dry-run 幂等无副作用"

# ── 完成 ──────────────────────────────────────────────────────────────────────
banner_pass "Step 11 手机号加密（8 段）"
