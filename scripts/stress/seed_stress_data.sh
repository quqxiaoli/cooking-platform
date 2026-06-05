#!/usr/bin/env bash
# scripts/stress/seed_stress_data.sh — 全面压测数据池生成器
#
# 注册 USER_COUNT 个用户 + 发 POST_COUNT 篇帖子，输出 3 个池文件：
#   /tmp/stress_pool_users.tsv   一行一用户：user_id\ttoken
#   /tmp/stress_pool_posts.tsv   一行一帖：  post_id\tscene_tag\tauthor_user_id
#   /tmp/stress_pool.json        canonical JSON（jq/人类可读）
#
# wrk lua 脚本（wrk_delete_like / wrk_get_scene_feed / wrk_get_user_posts /
# wrk_post_follow）会读 TSV，每条请求随机选 token + id，避免热点偏置。
#
# ⚠️ 前置条件（全面压测方案 §4.1 / §4.2）：
#   1) nginx 限流已临时关闭（否则 100 次 send-code 会被 30r/s + 1r/min 卡死）
#   2) 应用层 SMS per-IP 限流已临时关闭（否则同一出口 IP 只发得出 1-2 条码）
#
# 如未关闭以上两条，本脚本会快速失败并提示。
#
# 用法：
#   source .env.prod && bash scripts/stress/seed_stress_data.sh
#   # 自定义池大小：
#   USER_COUNT=200 POST_COUNT=100 bash scripts/stress/seed_stress_data.sh
#   # 清理上次的池数据（DB 中的用户/帖子也会清）：
#   bash scripts/stress/seed_stress_data.sh --cleanup

set -u

# ── 参数 ────────────────────────────────────────────────────────────────────
BASE="${BASE:-https://api.mellowck.com/api/v1}"
APP1_CONTAINER="${APP1_CONTAINER:-cooking-app1-prod}"
APP2_CONTAINER="${APP2_CONTAINER:-cooking-app2-prod}"
MYSQL_MASTER_CONTAINER="${MYSQL_MASTER_CONTAINER:-cooking-mysql-prod}"
MYSQL_ROOT_PW="${MYSQL_ROOT_PW:-${MYSQL_ROOT_PASSWORD:-}}"

USER_COUNT="${USER_COUNT:-100}"
POST_COUNT="${POST_COUNT:-50}"
PHONE_PREFIX="${PHONE_PREFIX:-139}"   # Step20 用 138 系，这里换 139 系做区分

# 5 个热点关键词，让 search 场景能命中
KEYWORDS=(verify stress test feed 搜索)

USERS_TSV="/tmp/stress_pool_users.tsv"
POSTS_TSV="/tmp/stress_pool_posts.tsv"
POOL_JSON="/tmp/stress_pool.json"

C_G='\033[0;32m'; C_R='\033[0;31m'; C_Y='\033[1;33m'; C_N='\033[0m'
ok()   { echo -e "${C_G}✓${C_N} $1"; }
bad()  { echo -e "${C_R}✗${C_N} $1"; }
warn() { echo -e "${C_Y}⚠${C_N} $1"; }
info() { echo -e "${C_Y}→${C_N} $1"; }
section() { echo ""; echo -e "${C_Y}── $1 ──${C_N}"; }

# ── 容器内 SQL helper ───────────────────────────────────────────────────────
mysql_q() {
  local sql="$1"
  docker exec "$MYSQL_MASTER_CONTAINER" \
    mysql -uroot -p"$MYSQL_ROOT_PW" -N -B -e "$sql" 2>/dev/null
}

# 抓某手机号最新一条 [SMS MOCK] 验证码（同 verify_step20.sh）
fetch_sms_code() {
  local phone="$1"
  local line
  line=$(
    { docker exec "$APP1_CONTAINER" sh -c "grep -F '\"phone\":\"$phone\"' /var/log/cooking/app.log" 2>/dev/null;
      docker exec "$APP2_CONTAINER" sh -c "grep -F '\"phone\":\"$phone\"' /var/log/cooking/app.log" 2>/dev/null; } \
      | grep -F '[SMS MOCK] verification code sent' \
      | tail -1
  )
  [ -z "$line" ] && echo "" && return
  echo "$line" | jq -r '.code'
}

# ════════════════════════════════════════════════════════════════════════════
# cleanup 子命令
# ════════════════════════════════════════════════════════════════════════════
if [ "${1:-}" = "--cleanup" ]; then
  section "Cleanup（清掉池文件 + DB 中 ${PHONE_PREFIX} 系测试数据）"
  if [ -z "$MYSQL_ROOT_PW" ]; then
    bad "MYSQL_ROOT_PW 未设置 —— 'source .env.prod' 后重试"
    exit 1
  fi
  rm -f "$USERS_TSV" "$POSTS_TSV" "$POOL_JSON"
  ok "已删除池文件"
  mysql_q "DELETE FROM cooking_platform.posts
           WHERE author_id IN (
             SELECT id FROM cooking_platform.users WHERE phone LIKE '${PHONE_PREFIX}%'
           );"
  mysql_q "DELETE FROM cooking_platform.users WHERE phone LIKE '${PHONE_PREFIX}%';"
  ok "已清掉 ${PHONE_PREFIX}* 用户 + 其名下帖子"
  exit 0
fi

# ════════════════════════════════════════════════════════════════════════════
# §1 前置检查
# ════════════════════════════════════════════════════════════════════════════
section "§1 前置检查"

command -v jq   >/dev/null && ok "jq 已安装"   || { bad "需要 jq";   exit 1; }
command -v curl >/dev/null && ok "curl 已安装" || { bad "需要 curl"; exit 1; }

if [ -z "$MYSQL_ROOT_PW" ]; then
  bad "MYSQL_ROOT_PW 未设置 —— 请先 'source .env.prod'"
  exit 1
fi
ok "MYSQL_ROOT_PW 已注入"

for cname in "$APP1_CONTAINER" "$APP2_CONTAINER" "$MYSQL_MASTER_CONTAINER"; do
  docker ps --format '{{.Names}}' | grep -q "^${cname}$" && ok "$cname running" \
    || { bad "$cname 未运行"; exit 1; }
done

if [ "$POST_COUNT" -gt "$USER_COUNT" ]; then
  bad "POST_COUNT($POST_COUNT) > USER_COUNT($USER_COUNT) —— 当前实现每用户最多 1 帖"
  exit 1
fi

# 探测 SMS per-IP 限流是否已关：发 2 条 send-code，间隔 1s，第 2 条应当成功
info "探测 SMS per-IP 限流状态..."
PROBE1="${PHONE_PREFIX}00000001"
PROBE2="${PHONE_PREFIX}00000002"
curl -sS -X POST "$BASE/auth/send-code" -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PROBE1\"}" >/dev/null
sleep 1
PROBE_RESP=$(curl -sS -X POST "$BASE/auth/send-code" -H 'Content-Type: application/json' \
  -d "{\"phone\":\"$PROBE2\"}")
PROBE_CODE=$(echo "$PROBE_RESP" | jq -r '.code // 0')
if [ "$PROBE_CODE" != "0" ]; then
  bad "SMS per-IP 限流仍在生效（探测响应 code=$PROBE_CODE）"
  bad "  请先按 全面压测方案 §4.2 关闭 sms per-IP 限流再重试"
  bad "  响应: $PROBE_RESP"
  exit 1
fi
ok "SMS per-IP 限流已关，可批量发码"

mkdir -p "$(dirname "$USERS_TSV")"

# ════════════════════════════════════════════════════════════════════════════
# §2 注册 USER_COUNT 个用户
# ════════════════════════════════════════════════════════════════════════════
section "§2 注册 $USER_COUNT 个用户（写 $USERS_TSV）"

echo -e "user_id\ttoken" > "$USERS_TSV"
USERS_JSON_PARTS=()  # for canonical pool.json

REG_FAIL=0
for i in $(seq 1 "$USER_COUNT"); do
  PHONE="${PHONE_PREFIX}$(printf '%08d' $((10000000 + i)))"

  curl -sS -X POST "$BASE/auth/send-code" \
    -H 'Content-Type: application/json' \
    -d "{\"phone\":\"$PHONE\"}" >/dev/null
  # SMS 写入日志是异步的，让 zap flush 一下
  sleep 0.15

  CODE=$(fetch_sms_code "$PHONE")
  if [ -z "$CODE" ]; then
    warn "user #$i ($PHONE) 抓码失败，跳过"
    REG_FAIL=$((REG_FAIL+1))
    continue
  fi

  LOGIN_RESP=$(curl -sS -X POST "$BASE/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"phone\":\"$PHONE\",\"code\":\"$CODE\"}")
  TOKEN=$(echo "$LOGIN_RESP" | jq -r '.data.access_token // empty')
  USER_ID=$(echo "$LOGIN_RESP" | jq -r '.data.user.id     // empty')
  if [ -z "$TOKEN" ] || [ -z "$USER_ID" ]; then
    warn "user #$i ($PHONE) 登录失败: $LOGIN_RESP"
    REG_FAIL=$((REG_FAIL+1))
    continue
  fi

  printf '%s\t%s\n' "$USER_ID" "$TOKEN" >> "$USERS_TSV"
  USERS_JSON_PARTS+=("$(jq -nc --arg p "$PHONE" --argjson id "$USER_ID" --arg t "$TOKEN" \
    '{phone:$p, id:$id, token:$t}')")

  # 每 10 个打一次进度
  if [ $((i % 10)) -eq 0 ]; then
    info "  进度: $i / $USER_COUNT 个用户"
  fi
done

USER_OK=$(( $(wc -l < "$USERS_TSV") - 1 ))
ok "用户注册完成: 成功 $USER_OK / 目标 $USER_COUNT （失败 $REG_FAIL）"

if [ "$USER_OK" -lt "$POST_COUNT" ]; then
  bad "成功用户数 ($USER_OK) < POST_COUNT ($POST_COUNT) —— 无法支撑发帖阶段"
  exit 1
fi

# ════════════════════════════════════════════════════════════════════════════
# §3 用前 POST_COUNT 个用户每人发 1 帖
# ════════════════════════════════════════════════════════════════════════════
section "§3 发布 $POST_COUNT 篇帖子（写 $POSTS_TSV）"

echo -e "post_id\tscene_tag\tauthor_user_id" > "$POSTS_TSV"
POSTS_JSON_PARTS=()

PUB_FAIL=0
LINE_NO=0
# 跳过 TSV 表头，逐行读用户
while IFS=$'\t' read -r USER_ID TOKEN; do
  LINE_NO=$((LINE_NO+1))
  if [ "$LINE_NO" -eq 1 ]; then continue; fi  # header
  POST_IDX=$((LINE_NO - 1))                   # 1-based 帖子序号
  if [ "$POST_IDX" -gt "$POST_COUNT" ]; then break; fi

  SCENE=$(( (POST_IDX - 1) % 8 + 1 ))
  KW="${KEYWORDS[$(( (POST_IDX - 1) % ${#KEYWORDS[@]} ))]}"
  TITLE="stress $KW post #$POST_IDX"
  CONTENT="stress seed: scene=$SCENE keyword=$KW author_uid=$USER_ID"

  BODY=$(jq -nc --arg t "$TITLE" --arg c "$CONTENT" --argjson s "$SCENE" \
    '{title:$t, scene_tag:$s, content:$c}')

  RESP=$(curl -sS -X POST "$BASE/posts" \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -d "$BODY")
  POST_ID=$(echo "$RESP" | jq -r '.data.post_id // empty')
  if [ -z "$POST_ID" ]; then
    warn "post #$POST_IDX (uid=$USER_ID) 失败: $RESP"
    PUB_FAIL=$((PUB_FAIL+1))
    continue
  fi

  printf '%s\t%s\t%s\n' "$POST_ID" "$SCENE" "$USER_ID" >> "$POSTS_TSV"
  POSTS_JSON_PARTS+=("$(jq -nc --argjson id "$POST_ID" --argjson s "$SCENE" --argjson a "$USER_ID" --arg t "$TITLE" \
    '{id:$id, scene_tag:$s, author_id:$a, title:$t}')")

  if [ $((POST_IDX % 10)) -eq 0 ]; then
    info "  进度: $POST_IDX / $POST_COUNT 篇帖子"
  fi
done < "$USERS_TSV"

POST_OK=$(( $(wc -l < "$POSTS_TSV") - 1 ))
ok "发帖完成: 成功 $POST_OK / 目标 $POST_COUNT （失败 $PUB_FAIL）"

# ════════════════════════════════════════════════════════════════════════════
# §4 写 canonical JSON
# ════════════════════════════════════════════════════════════════════════════
section "§4 写 canonical pool ($POOL_JSON)"

USERS_ARR=$(IFS=,; echo "${USERS_JSON_PARTS[*]}")
POSTS_ARR=$(IFS=,; echo "${POSTS_JSON_PARTS[*]}")
jq -n \
  --argjson users "[$USERS_ARR]" \
  --argjson posts "[$POSTS_ARR]" \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{generated_at:$generated_at, users:$users, posts:$posts}' \
  > "$POOL_JSON"
ok "pool JSON 已写: $(wc -c < "$POOL_JSON") bytes"

# ════════════════════════════════════════════════════════════════════════════
# §5 汇总
# ════════════════════════════════════════════════════════════════════════════
section "汇总"
printf "  USERS:  %d ok / %d failed → %s\n" "$USER_OK" "$REG_FAIL" "$USERS_TSV"
printf "  POSTS:  %d ok / %d failed → %s\n" "$POST_OK" "$PUB_FAIL" "$POSTS_TSV"
printf "  JSON :  %s\n" "$POOL_JSON"
echo ""
echo "下一步：跑全面压测方案 §6 Phase 1 冒烟"
echo "  WRK_RATE=10 WRK_DURATION=60s bash scripts/stress_test.sh"
echo ""
echo "回滚（恢复生产）：bash scripts/stress/seed_stress_data.sh --cleanup"

# 至少 80% 成功才算 OK
MIN_OK=$(( USER_COUNT * 80 / 100 ))
if [ "$USER_OK" -lt "$MIN_OK" ]; then
  bad "用户注册成功率 < 80%，可能 SMS 限流仍在或日志抓码异常"
  exit 1
fi
exit 0
