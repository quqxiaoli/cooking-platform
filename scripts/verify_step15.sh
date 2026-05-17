#!/usr/bin/env bash
# verify_step15.sh — Step 15: Nginx 双实例负载均衡验证
# Usage: bash scripts/verify_step15.sh
set -euo pipefail
source ./scripts/verify_common.sh

NGINX_URL="http://localhost"
APP1_CONTAINER="cooking-app1-dev"
APP2_CONTAINER="cooking-app2-dev"
NGINX_CONTAINER="cooking-nginx-dev"
REQUEST_COUNT=100

# ── §1 编译检查 ────────────────────────────────────────────────────────────────
section "§1 编译检查"
go build ./... && ok "go build 通过"

# ── §2 容器健康状态 ────────────────────────────────────────────────────────────
section "§2 容器健康状态"
# app1/app2 have Docker healthchecks; nginx does not (it's checked via HTTP in §3)
for cname in "$APP1_CONTAINER" "$APP2_CONTAINER"; do
  status=$(docker inspect --format='{{.State.Health.Status}}' "$cname" 2>/dev/null || echo "missing")
  if [ "$status" = "healthy" ]; then
    ok "$cname: healthy"
  else
    fail "$cname: $status"
  fi
done
# Nginx: verify the container is running (no healthcheck configured)
nginx_running=$(docker inspect --format='{{.State.Running}}' "$NGINX_CONTAINER" 2>/dev/null || echo "false")
[ "$nginx_running" = "true" ] && ok "$NGINX_CONTAINER: running" || fail "$NGINX_CONTAINER: not running"

# ── §3 /health/ready 路由可达 ─────────────────────────────────────────────────
section "§3 /health/ready 路由可达"
http_code=$(curl -s -o /dev/null -w "%{http_code}" "${NGINX_URL}/health/ready")
[ "$http_code" = "200" ] && ok "/health/ready → HTTP $http_code" || fail "/health/ready → HTTP $http_code (expected 200)"

# ── §4 /readiness 路由保持可达（向后兼容）────────────────────────────────────
section "§4 /readiness 路由保持可达"
http_code=$(curl -s -o /dev/null -w "%{http_code}" "${NGINX_URL}/readiness")
[ "$http_code" = "200" ] && ok "/readiness → HTTP $http_code" || fail "/readiness → HTTP $http_code (expected 200)"

# ── §5 100 次请求均匀分布到两实例 ────────────────────────────────────────────
section "§5 负载均衡分布验证（${REQUEST_COUNT} 次请求）"

# Nginx Docker 镜像的 /var/log/nginx/access.log 是 /dev/stdout 的符号链接，
# 实际日志输出到 docker logs。用行数差值法截取本次请求产生的日志。
app1_ip=$(docker inspect --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$APP1_CONTAINER")
app2_ip=$(docker inspect --format='{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$APP2_CONTAINER")

lines_before=$(docker logs "$NGINX_CONTAINER" 2>&1 | wc -l)

for i in $(seq 1 "$REQUEST_COUNT"); do
  curl -s -o /dev/null "${NGINX_URL}/health/ready"
done

lines_after=$(docker logs "$NGINX_CONTAINER" 2>&1 | wc -l)
new_lines=$(( lines_after - lines_before ))

log_output=$(docker logs "$NGINX_CONTAINER" 2>&1 | tail -n "$new_lines")

count_app1=$(echo "$log_output" | grep -c "upstream=${app1_ip}:8080" || true)
count_app2=$(echo "$log_output" | grep -c "upstream=${app2_ip}:8080" || true)
total=$((count_app1 + count_app2))

echo "  app1 (${app1_ip}): ${count_app1} 次"
echo "  app2 (${app2_ip}): ${count_app2} 次"
echo "  合计: ${total} / ${REQUEST_COUNT}"

[ "$total" -eq "$REQUEST_COUNT" ] && ok "所有请求都命中了 upstream" || fail "部分请求未记录到 access log (total=${total})"

# 分布检验：两实例各占 30%~70% 视为均匀（round-robin 理论值 50%）
min_expected=$(( REQUEST_COUNT * 30 / 100 ))
if [ "$count_app1" -ge "$min_expected" ] && [ "$count_app2" -ge "$min_expected" ]; then
  ok "分布均匀：app1=${count_app1} app2=${count_app2}（各占 30%~70% 范围内）"
else
  fail "分布不均匀：app1=${count_app1} app2=${count_app2}（期望各自 ≥ ${min_expected}）"
fi

# ── §6 Nginx 配置语法检查 ────────────────────────────────────────────────────
section "§6 Nginx 配置语法检查"
# nginx -t writes to stderr, capture via docker logs after the test
nginx_test=$(docker exec cooking-nginx-dev nginx -t 2>&1 || true)
echo "$nginx_test" | grep -q "syntax is ok" && ok "nginx -t: syntax is ok" || fail "nginx -t 失败: $nginx_test"

echo ""
echo "============================="
echo " Step 15 验证全部通过 ✓"
echo "============================="
