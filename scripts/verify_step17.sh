#!/usr/bin/env bash
# scripts/verify_step17.sh — Step 17 CI/CD 验证
# 验证三个 GitHub Actions workflow 文件存在、YAML 合法、Go 项目可正常构建。

set -euo pipefail
source "$(dirname "$0")/verify_common.sh"

section "§1 Workflow 文件存在性检查"
[ -f ".github/workflows/pr.yml" ]      && ok "pr.yml 存在"      || fail "pr.yml 不存在"
[ -f ".github/workflows/build.yml" ]   && ok "build.yml 存在"   || fail "build.yml 不存在"
[ -f ".github/workflows/release.yml" ] && ok "release.yml 存在" || fail "release.yml 不存在"

section "§2 YAML 语法验证"
for f in .github/workflows/pr.yml .github/workflows/build.yml .github/workflows/release.yml; do
  if ruby -e "require 'yaml'; YAML.safe_load(File.read('$f'))" 2>/dev/null; then
    ok "$f YAML 合法"
  else
    fail "$f YAML 解析失败"
  fi
done

section "§3 Workflow 触发器验证"
grep -q "pull_request"       .github/workflows/pr.yml      && ok "pr.yml 触发器: pull_request"   || fail "pr.yml 缺少 pull_request 触发器"
grep -q "branches: \[main\]" .github/workflows/pr.yml      && ok "pr.yml 目标分支: main"          || fail "pr.yml 缺少 branches: [main]"
grep -q "push"               .github/workflows/build.yml   && ok "build.yml 触发器: push"         || fail "build.yml 缺少 push 触发器"
grep -q "tags:"              .github/workflows/release.yml  && ok "release.yml 触发器: tags"       || fail "release.yml 缺少 tags 触发器"

section "§4 pr.yml services 块（MySQL + Redis）"
grep -q "mysql:8.0" .github/workflows/pr.yml && ok "MySQL service 已配置" || fail "pr.yml 缺少 MySQL service"
grep -q "redis:7.2" .github/workflows/pr.yml && ok "Redis service 已配置"  || fail "pr.yml 缺少 Redis service"

section "§5 go build + go vet 本地验证"
go build ./... && ok "go build ./... 通过" || fail "go build ./... 失败"
go vet ./...   && ok "go vet ./... 通过"   || fail "go vet ./... 失败"

section "§6 集成测试文件存在"
[ -f "internal/integration/ping_test.go" ] && ok "ping_test.go 存在" || fail "ping_test.go 不存在"
grep -q "TestMySQLPing" internal/integration/ping_test.go && ok "TestMySQLPing 已定义" || fail "TestMySQLPing 未找到"
grep -q "TestRedisPing"  internal/integration/ping_test.go && ok "TestRedisPing 已定义"  || fail "TestRedisPing 未找到"

section "§7 单元测试全部通过"
go test -count=1 ./internal/middleware/... ./internal/event/... ./pkg/crypto/... \
  && ok "单元测试 (middleware / event / crypto) 全部通过" \
  || fail "单元测试失败"

section "§8 集成测试本地 SKIP（无 TEST_MYSQL_DSN）"
result=$(go test -v -count=1 ./internal/integration/... 2>&1)
echo "$result" | grep -q "SKIP" && ok "集成测试在本地正确 SKIP" || fail "集成测试未跳过（可能意外失败）"

banner_pass "Step 17 CI/CD"
