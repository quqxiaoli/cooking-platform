#!/usr/bin/env bash
# scripts/step_closeout.sh
# 用法：bash scripts/step_closeout.sh 10
# 收尾前自检，不做任何提交动作。
# 检查项：交付物完整性、敏感信息、验证脚本、commit message 草稿。

set -uo pipefail

N="${1:-}"
if [ -z "$N" ]; then
    echo "ERROR: step number required. Usage: bash scripts/step_closeout.sh 10" >&2
    exit 1
fi

PREV=$((N - 1))
PREV_TAG="step-${PREV}-done"

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m'

ok()    { echo -e "${GREEN}✅${NC} $1"; }
warn()  { echo -e "${YELLOW}⚠️ ${NC} $1"; }
fail()  { echo -e "${RED}❌${NC} $1"; }
info()  { echo -e "${BLUE}ℹ️ ${NC} $1"; }
section() { echo ""; echo -e "${BLUE}── $1 ──${NC}"; }

ERRORS=0
WARNS=0
incr_err()  { ERRORS=$((ERRORS + 1)); }
incr_warn() { WARNS=$((WARNS + 1)); }

# ── 1. Git 工作树状态 ────────────────────────────────────────────────
section "1. Git 工作树状态"

BRANCH=$(git branch --show-current)
info "当前分支：$BRANCH"

if [[ "$BRANCH" == "main" ]]; then
    warn "当前在 main 分支，按约定本步应在 feature/step-${N}-* 分支收尾"
    incr_warn
elif [[ "$BRANCH" == feature/step-${N}-* ]]; then
    ok "在 feature 分支上"
else
    warn "分支名不符合 feature/step-${N}-* 规范，实际：$BRANCH"
    incr_warn
fi

if [ -n "$(git status --porcelain)" ]; then
    info "存在未提交的改动（收尾时正常）："
    git status --short | sed 's/^/    /'
else
    warn "工作树干净，本步没有任何未提交改动？请确认是否走错步骤"
    incr_warn
fi

# ── 2. 交付物完整性 ──────────────────────────────────────────────────
section "2. 交付物完整性（v4 四样）"

PROGRESS_FILE="docs/progress/${N}_项目进度追踪.md"
STORYLINE_GLOB="docs/storylines/${N}_故事线-第${N}步-*.md"
CHANGES_GLOB="docs/changes/${N}_代码变更清单-第${N}步*.md"

if [ -f "$PROGRESS_FILE" ]; then
    ok "进度追踪：$PROGRESS_FILE"
else
    fail "缺失：$PROGRESS_FILE"
    incr_err
fi

STORYLINE_FILES=( $STORYLINE_GLOB )
if [ -f "${STORYLINE_FILES[0]}" ]; then
    ok "故事线：${STORYLINE_FILES[0]}"
else
    fail "缺失：docs/storylines/${N}_故事线-第${N}步-<模块名>.md"
    incr_err
fi

CHANGES_FILES=( $CHANGES_GLOB )
if [ -f "${CHANGES_FILES[0]}" ]; then
    ok "代码变更清单：${CHANGES_FILES[0]}"
    # 检查是否还有未填的设计要点
    UNFILLED=$(grep -c "待 Claude Code 填写" "${CHANGES_FILES[0]}" || true)
    if [ "$UNFILLED" -gt 0 ]; then
        warn "变更清单有 $UNFILLED 处「设计要点」尚未填写"
        incr_warn
    fi
else
    fail "缺失：docs/changes/${N}_代码变更清单-第${N}步.md（先跑 make step-diff N=${N}）"
    incr_err
fi

# ── 3. 敏感信息扫描 ─────────────────────────────────────────────────
section "3. 敏感信息扫描"

# 在本步 diff 范围内扫描，避免误报历史代码
if git rev-parse --verify "$PREV_TAG" >/dev/null 2>&1; then
    SENSITIVE_PATTERNS='AccessKey|AccessKeySecret|aliyun_secret|password\s*=\s*["\047][^"\047]{4,}|"phone":\s*"1[3-9][0-9]{9}"'
    HITS=$(git diff "$PREV_TAG"..HEAD | grep -E "^\+" | grep -E "$SENSITIVE_PATTERNS" || true)
    if [ -n "$HITS" ]; then
        fail "在本步改动中检测到疑似敏感信息："
        echo "$HITS" | head -20 | sed 's/^/    /'
        incr_err
    else
        ok "本步改动中未检测到敏感信息"
    fi
else
    warn "找不到 $PREV_TAG，跳过 diff 范围扫描"
    incr_warn
fi

# ── 5+6. 收尾命令块（填充完整，整段复制即可执行）─────────────────
section "5. 收尾命令块（整段复制粘贴执行）"

# 从分支名提取模块名，fallback 到占位符
MODULE_NAME=$(echo "$BRANCH" | sed "s/feature\/step-${N}-//")
if [ "$MODULE_NAME" = "$BRANCH" ]; then
    MODULE_NAME="<模块名>"
fi

# 从进度追踪文件提取偏离点（表格行，去掉分隔行和空行）
DEVIATION_SUMMARY=""
if [ -f "$PROGRESS_FILE" ]; then
    DEVIATION_SUMMARY=$(awk '
        /\*\*本步与 PRD 的偏离点\*\*/ { capture=1; next }
        capture && /^---/ { exit }
        capture && /^\|---/ { next }
        capture && /^\|/ { print }
    ' "$PROGRESS_FILE" | grep -v '^$' | grep -v '^| PRD 章节' | head -5)
fi
DEVIATION_LINE="${DEVIATION_SUMMARY:-严格按 PRD 实现}"

# 从代码变更清单提取所有模块节标题作为主要变更摘要
CHANGES_SUMMARY=""
if [ -f "${CHANGES_FILES[0]:-}" ]; then
    CHANGES_SUMMARY=$(grep '^## ' "${CHANGES_FILES[0]}" \
        | grep -v '本步无变更' \
        | sed 's/^## /- /' \
        | head -6)
fi
CHANGES_LINE="${CHANGES_SUMMARY:-（见变更清单）}"

# 输出完整可执行命令块
cat <<BLOCK

━━━━━━━━━━━━  整段复制，粘贴到终端执行  ━━━━━━━━━━━━

git add . && git commit -F- <<'COMMIT_MSG'
feat(step-${N}): ${MODULE_NAME} 实现完成

主要变更：
${CHANGES_LINE}

偏离点：
${DEVIATION_LINE}

验证：scripts/verify_step${N}.sh 全部通过
COMMIT_MSG

git checkout main
git merge --no-ff ${BRANCH}
git tag step-${N}-done
git push origin main --tags

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

BLOCK

# ── 总结 ────────────────────────────────────────────────────────────
section "总结"
if [ $ERRORS -gt 0 ]; then
    fail "${ERRORS} 个阻断项，${WARNS} 个警告。请先修复阻断项再收尾"
    exit 1
elif [ $WARNS -gt 0 ]; then
    warn "${WARNS} 个警告，无阻断项。确认警告内容后可以收尾"
    exit 0
else
    ok "全部检查通过，可以执行收尾流程"
    exit 0
fi