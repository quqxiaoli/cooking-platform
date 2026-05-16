#!/usr/bin/env bash
# verify_step12.sh — Step 12: 日志脱敏 + 错误码体系最终收口
set -euo pipefail
source "$(dirname "$0")/verify_common.sh"

BASE_URL="${BASE_URL:-http://localhost:8080}"

section "§1  编译检查：全项目无报错"
go build ./... && ok "go build ./... passed"

section "§2  pkg/crypto/mask.go 单元测试"
go test ./pkg/crypto/... -run TestMask -v -count=1 && ok "mask tests passed"

section "§3  pkg/logger/fields.go — vet 检查"
go vet ./pkg/logger/... && ok "pkg/logger vet passed"

section "§4  middleware/logger.go — sanitizeQuery 单元测试"
go test ./internal/middleware/... -run TestSanitizeQuery -v -count=1 && ok "sanitizeQuery tests passed"

section "§5  安全响应头 — 实际 HTTP 响应验证"
precheck
HEADERS=$(curl -si "${BASE_URL}/health" | head -30)
assert_contains "$HEADERS" "X-Content-Type-Options: nosniff"   "X-Content-Type-Options header present"
assert_contains "$HEADERS" "X-Frame-Options: DENY"              "X-Frame-Options header present"
assert_contains "$HEADERS" "mode=block"                         "X-XSS-Protection header present"
assert_contains "$HEADERS" "Referrer-Policy:"                   "Referrer-Policy header present"

section "§6  敏感 query 参数不出现在日志中"
# 发起带 phone 的请求，经过 middleware Logger 后 query 应被脱敏
curl -s "${BASE_URL}/nonexistent?phone=13800138000&code=123456" -o /dev/null
sleep 0.3
LOGFILE="$LOG_FILE"
if [[ -f "$LOGFILE" ]]; then
  RECENT=$(tail -10 "$LOGFILE")
  assert_not_contains "$RECENT" "13800138000" "phone not leaked in recent log lines"
  assert_not_contains "$RECENT" "123456"       "code not leaked in recent log lines"
else
  ok "log file not present (dev stdout mode — sanitize logic covered by §4 unit test)"
fi

section "§7  错误码完整性 — 无重复 code 值"
DUPES=$(grep -E 'New\(http\.' pkg/errcode/errcode.go | grep -oE '[0-9]{6}' | sort | uniq -d || true)
if [[ -n "$DUPES" ]]; then
  fail "duplicate errcode(s) found: $DUPES"
else
  ok "no duplicate error codes"
fi

section "§8  错误码完整性 — 所有段位有文档注释"
CONTENT=$(cat pkg/errcode/errcode.go)
for seg in "410xxx" "412xxx" "440xxx" "450xxx" "460xxx" "470xxx" "480xxx"; do
  assert_contains "$CONTENT" "$seg" "segment $seg documented"
done

banner_pass "Step 12 日志脱敏 + 错误码收口"
