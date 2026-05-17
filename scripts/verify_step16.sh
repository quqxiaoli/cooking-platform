#!/usr/bin/env bash
# verify_step16.sh — Step 16: Prometheus + Grafana 监控验证
# Usage: bash scripts/verify_step16.sh
set -euo pipefail
source ./scripts/verify_common.sh

APP1_URL="http://localhost:8080"   # direct (if running go run locally)
NGINX_URL="http://localhost"       # through Nginx when using Docker
PROM_URL="http://localhost:9090"
GRAFANA_URL="http://localhost:3000"
PROM_CONTAINER="cooking-prometheus-dev"
GRAFANA_CONTAINER="cooking-grafana-dev"

# Detect whether we are in Docker mode or local mode.
# Use Nginx if app1 is not directly reachable on 8080.
BASE_URL="$NGINX_URL"
if curl -sf "$APP1_URL/health" >/dev/null 2>&1; then
  BASE_URL="$APP1_URL"
fi

# ── §1 编译检查 ────────────────────────────────────────────────────────────────
section "§1 编译检查"
go build ./... && ok "go build 通过"

# ── §2 /metrics 端点可达 ──────────────────────────────────────────────────────
section "§2 /metrics 端点返回 Prometheus text format"
METRICS_BODY=$(curl -sf "$BASE_URL/metrics" 2>/dev/null || true)
if [ -z "$METRICS_BODY" ]; then
  # fallback: try app1 direct if Nginx doesn't proxy /metrics (by design)
  METRICS_BODY=$(curl -sf "http://localhost:8080/metrics" 2>/dev/null || true)
fi
if [ -z "$METRICS_BODY" ]; then
  # Try app1 container directly
  METRICS_BODY=$(docker exec cooking-app1-dev wget -qO- http://localhost:8080/metrics 2>/dev/null || true)
fi
if echo "$METRICS_BODY" | grep -q "cooking_http_requests_total"; then
  ok "/metrics 包含 cooking_http_requests_total"
else
  fail "/metrics 未包含 cooking_http_requests_total"
fi
if echo "$METRICS_BODY" | grep -q "cooking_consumer_queue_depth"; then
  ok "/metrics 包含 cooking_consumer_queue_depth"
else
  fail "/metrics 未包含 cooking_consumer_queue_depth"
fi
if echo "$METRICS_BODY" | grep -q "cooking_redis_command_duration_seconds"; then
  ok "/metrics 包含 cooking_redis_command_duration_seconds"
else
  fail "/metrics 未包含 cooking_redis_command_duration_seconds"
fi
if echo "$METRICS_BODY" | grep -q "cooking_mysql_pool_open_connections"; then
  ok "/metrics 包含 cooking_mysql_pool_open_connections"
else
  fail "/metrics 未包含 cooking_mysql_pool_open_connections"
fi

# ── §3 Prometheus 容器健康 + 可达 ──────────────────────────────────────────────
section "§3 Prometheus 容器健康"
if docker ps --filter "name=$PROM_CONTAINER" --filter "status=running" | grep -q "$PROM_CONTAINER" 2>/dev/null; then
  ok "$PROM_CONTAINER running"
  # Check Prometheus API
  PROM_STATUS=$(curl -sf "$PROM_URL/-/healthy" 2>/dev/null || echo "unreachable")
  if [ "$PROM_STATUS" = "Prometheus Server is Healthy." ]; then
    ok "Prometheus /-/healthy → OK"
  else
    warn "Prometheus /-/healthy: $PROM_STATUS (可能刚启动)"
  fi
else
  warn "$PROM_CONTAINER 未运行（跳过 Docker 检查）"
fi

# ── §4 Prometheus 已发现 scrape targets ────────────────────────────────────────
section "§4 Prometheus scrape targets"
TARGETS=$(curl -sf "$PROM_URL/api/v1/targets" 2>/dev/null || echo "")
if echo "$TARGETS" | grep -q '"health":"up"'; then
  UP_COUNT=$(echo "$TARGETS" | python3 -c "import sys,json; d=json.load(sys.stdin); print(sum(1 for t in d['data']['activeTargets'] if t['health']=='up'))" 2>/dev/null || echo "?")
  ok "Prometheus targets up: $UP_COUNT"
else
  warn "Prometheus targets 状态未知（容器可能未启动）"
fi

# ── §5 Grafana 容器健康 ─────────────────────────────────────────────────────────
section "§5 Grafana 容器健康"
if docker ps --filter "name=$GRAFANA_CONTAINER" --filter "status=running" | grep -q "$GRAFANA_CONTAINER" 2>/dev/null; then
  ok "$GRAFANA_CONTAINER running"
  GRAFANA_HEALTH=$(curl -sf "$GRAFANA_URL/api/health" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('database','?'))" 2>/dev/null || echo "unreachable")
  if [ "$GRAFANA_HEALTH" = "ok" ]; then
    ok "Grafana /api/health → database=ok"
  else
    warn "Grafana /api/health: $GRAFANA_HEALTH"
  fi
else
  warn "$GRAFANA_CONTAINER 未运行（跳过 Docker 检查）"
fi

# ── §6 Prometheus 采集到 cooking 指标 ──────────────────────────────────────────
section "§6 Prometheus 已采集 cooking_http_requests_total"
QUERY_RESULT=$(curl -sf "$PROM_URL/api/v1/query?query=cooking_http_requests_total" 2>/dev/null || echo "")
if echo "$QUERY_RESULT" | grep -q '"resultType"'; then
  ok "Prometheus query cooking_http_requests_total 返回结果"
else
  warn "Prometheus 查询失败（容器可能未启动或尚未完成首次抓取）"
fi

echo ""
echo "Step 16 验证完成。"
echo "  Prometheus UI: $PROM_URL"
echo "  Grafana UI:    $GRAFANA_URL  (admin / admin)"
