#!/usr/bin/env bash
# scripts/check_config_parity.sh
#
# 校验三份 yaml（dev / docker / prod）的顶级 key 集合一致。
# 任意一份缺段必触发非零退出，CI 用来挡住 "prod 漏配 consumer/cache" 类事故。
#
# 用法：bash scripts/check_config_parity.sh
# 依赖：yq (https://github.com/mikefarah/yq) v4+；本地无则降级到 grep 抓顶级 key。
set -euo pipefail

CFG_DIR="$(cd "$(dirname "$0")/.." && pwd)/configs"
FILES=(config.yaml config.docker.yaml config.prod.yaml)

extract_keys() {
  local f="$1"
  if command -v yq >/dev/null 2>&1; then
    yq eval 'keys | .[]' "$f" 2>/dev/null | sort -u
  else
    # Fallback: lines that start at column 0 with letter and end with ":".
    grep -E '^[a-zA-Z_][a-zA-Z0-9_]*:' "$f" | sed 's/:.*//' | sort -u
  fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

for f in "${FILES[@]}"; do
  path="$CFG_DIR/$f"
  [[ -f "$path" ]] || { echo "ERROR: missing $path" >&2; exit 1; }
  extract_keys "$path" > "$TMP/$f.keys"
done

UNION="$TMP/union"
cat "$TMP"/*.keys | sort -u > "$UNION"

EXIT=0
for f in "${FILES[@]}"; do
  missing="$(comm -23 "$UNION" "$TMP/$f.keys" || true)"
  if [[ -n "$missing" ]]; then
    echo "FAIL: $f missing top-level sections:" >&2
    echo "$missing" | sed 's/^/  - /' >&2
    EXIT=1
  fi
done

if [[ $EXIT -eq 0 ]]; then
  echo "OK: all three configs share the same top-level sections."
fi
exit "$EXIT"
