#!/bin/bash
# 收集 Step 7 开工需要的 A 组业务文件
# 用法：cd cooking-platform && bash scripts/collect_step7_files.sh

OUTPUT="/tmp/step7_a_files.md"
> "$OUTPUT"

FILES=(
  "internal/model/post.go"
  "internal/model/user.go"
  "internal/repository/post_repository.go"
  "internal/service/post_service.go"
  "internal/handler/post.go"
  "internal/handler/feed.go"
  "internal/handler/dto/post_dto.go"
  "internal/dto/post.go"
  "internal/handler/router.go"
  "scripts/verify_step5.sh"
  "scripts/verify_step6.sh"
)

for f in "${FILES[@]}"; do
  if [ -f "$f" ]; then
    echo ""                       >> "$OUTPUT"
    echo "--- $f ---"             >> "$OUTPUT"
    echo '```'                    >> "$OUTPUT"
    cat "$f"                      >> "$OUTPUT"
    echo '```'                    >> "$OUTPUT"
    echo "[ok]   $f"
  else
    echo "[skip] $f not found"
  fi
done

echo ""
echo "==== 以下文件存在但脚本没收集，可能也要贴 ===="
find internal -type f \( -name "*post*" -o -name "*feed*" -o -name "*search*" \) 2>/dev/null | \
  while read line; do
    skip=0
    for f in "${FILES[@]}"; do
      [ "$line" = "$f" ] && skip=1 && break
    done
    [ $skip -eq 0 ] && echo "  $line"
  done

echo ""
echo "✅ 输出已写入 $OUTPUT"
echo "📋 行数：$(wc -l < $OUTPUT)"
echo "📋 复制到剪贴板（macOS）：pbcopy < $OUTPUT"