#!/usr/bin/env bash
# scripts/gen_step_diff.sh
# 用法：bash scripts/gen_step_diff.sh 10
# 生成 docs/changes/10_代码变更清单-第10步.md 脚手架
# 基于 git diff step-(N-1)-done..HEAD 自动分类文件

set -uo pipefail

N="${1:-}"
if [ -z "$N" ]; then
    echo "ERROR: step number required. Usage: bash scripts/gen_step_diff.sh 10" >&2
    exit 1
fi

PREV=$((N - 1))
PREV_TAG="step-${PREV}-done"
OUTPUT="docs/changes/${N}_代码变更清单-第${N}步.md"

# 检查上一步 tag 是否存在
if ! git rev-parse --verify "$PREV_TAG" >/dev/null 2>&1; then
    echo "ERROR: tag $PREV_TAG not found. Did Step $PREV finish properly?" >&2
    exit 1
fi

# 确保 docs/changes 目录存在
mkdir -p docs/changes

# 拉取 diff 文件列表（A=新增, M=修改, D=删除, R=重命名）
DIFF_RAW=$(git diff "$PREV_TAG"..HEAD --name-status)

if [ -z "$DIFF_RAW" ]; then
    echo "WARN: no changes between $PREV_TAG and HEAD" >&2
    # 仍然生成一个空脚手架文件，方便后续手动填
fi

# 分类函数
classify() {
    local path="$1"
    case "$path" in
        migrations/*|internal/repository/*|internal/model/*) echo "数据层" ;;
        internal/service/*|internal/cache/*|internal/consumer/*|internal/event/*) echo "业务层" ;;
        internal/handler/*|internal/middleware/*|cmd/*) echo "接入层" ;;
        pkg/*) echo "公共库" ;;
        configs/*) echo "配置" ;;
        scripts/*) echo "脚本" ;;
        docs/*) echo "文档" ;;
        *) echo "其他" ;;
    esac
}

# 变更类型映射
status_label() {
    case "$1" in
        A) echo "新增" ;;
        M) echo "修改" ;;
        D) echo "删除" ;;
        R*) echo "重命名" ;;
        *) echo "其他" ;;
    esac
}

# 收集分类（用临时文件按层归类）
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

while IFS=$'\t' read -r status file rest; do
    [ -z "$status" ] && continue
    [ -z "$file" ] && continue
    # 跳过 docs/changes 本身（避免本文件出现在自己里）
    case "$file" in
        docs/changes/*) continue ;;
    esac
    layer=$(classify "$file")
    label=$(status_label "$status")
    echo "${file}|${label}" >> "$TMPDIR/$layer"
done <<< "$DIFF_RAW"

# 输出 markdown
{
    echo "# 第 ${N} 步 · 代码变更清单"
    echo ""
    echo "> 自动生成于 $(date '+%Y-%m-%d %H:%M:%S')，基于 \`git diff ${PREV_TAG}..HEAD\`"
    echo "> Claude Code 在每个文件条目下填写「设计要点」（80 字内）"
    echo ""
    echo "---"
    echo ""

    for layer in 数据层 业务层 接入层 公共库 配置 脚本 文档 其他; do
        f="$TMPDIR/$layer"
        [ ! -f "$f" ] && continue
        echo "## ${layer}"
        echo ""
        while IFS='|' read -r file label; do
            echo "- \`${file}\`（${label}）"
            echo "  - 设计要点：（待 Claude Code 填写，80 字内说明为什么这么设计）"
            echo ""
        done < "$f"
    done

    echo "---"
    echo ""
    echo "## 统计"
    echo ""
    TOTAL=$(echo "$DIFF_RAW" | grep -v '^$' | wc -l | tr -d ' ')
    A_COUNT=$(echo "$DIFF_RAW" | grep -c '^A' || true)
    M_COUNT=$(echo "$DIFF_RAW" | grep -c '^M' || true)
    D_COUNT=$(echo "$DIFF_RAW" | grep -c '^D' || true)
    echo "- 变更文件总数：${TOTAL}"
    echo "- 新增：${A_COUNT}"
    echo "- 修改：${M_COUNT}"
    echo "- 删除：${D_COUNT}"
} > "$OUTPUT"

echo "✅ 生成完成：$OUTPUT"
echo ""
echo "下一步："
echo "  1. Claude Code 阅读该文件，逐条填写「设计要点」"
echo "  2. 收尾时 git add 一并提交"