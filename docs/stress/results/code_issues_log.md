# 全面压测 — 代码问题记录（业务代码暴露的问题）

> **创建日期**：2026-06-05
> **范围**：压测期间暴露的**业务代码 / 配置 / 架构**问题（不是压测脚本自身的 bug）。
> **维护规则**：发现一条记一条；**不在压测期内改代码**，全部压测结束（Phase 9 回滚后）再统一评估优先级、立 PR、合入 v2 路线图。
> **配套文件**：压测脚本 / 流程 / 工具自身的 bug 记到 `stress_issues_log.md`，不混到本文件里。

---

## #C1 审核 Consumer 命中主从复制延迟 → 50%+ post 永远停留 audit_status=0

- **发现于**：Phase 1 冒烟前 seed 阶段（2026-06-05 10:03~10:06），Phase 1 v3 复现于 stress_test.sh §2 setup post 63
- **症状**：
  - seed 创建 50 个 post 后查 DB，55/61 条 `audit_status=0`（pending），仅 6 条 `audit_status=1`
  - stress_test.sh §2 每次新创建的 setup post 同样卡 `audit_status=0, is_visible=0` → Phase 1 v3 中 detail 场景 100% 404、like 场景 55% 404
- **app 日志**：
  ```
  audit consumer: load post failed post_id=23 error=post not found
  audit consumer: load post failed post_id=25 error=post not found
  ...（奇数/偶数都有，约 90% 失败）
  audit consumer: post reviewed post_id=35 audit_status=1 is_visible=1  ← 偶发成功
  ```
- **根因**：
  - master DB INSERT post 后，service 立刻 publish audit 事件到 MQ
  - audit consumer 取到事件后用 `db.First(&post, post_id)` 查 post
  - 当前 GORM DBResolver 用 Random Policy 偏 slave（KPI 验证 91% 读走 slave）
  - 高并发写入时 slave 复制延迟 > consumer 拉取间隔 → consumer 查 slave 拿不到 → "post not found" 警告 → ACK 而不回填 audit_status
  - 单条 post 时延迟窗口小所以 Step 18-20 验收没暴露；seed 一秒发多个就触发；stress 单 post setup 也偶发触发
- **影响**：
  - audit_status=0 的 post 不出现在 feed/search/detail（被 service 过滤）
  - 任何依赖批量发帖的压测场景都被劫持
  - stress_test.sh setup 阶段创建的 detail/like 测试 post 不可用
- **建议修复**（v2，**不在压测期内做**）：
  - `internal/consumer/audit_consumer.go` 查 post 时强制走 master：`db.Clauses(dbresolver.Write).First(&post, id)`（GORM DBResolver 提供 Write Hint）
  - 或者改成 retry-with-backoff：第一次 not found 时 sleep 100ms 重试 1-2 次
- **当前 workaround**：
  ```sql
  UPDATE cooking_platform.posts SET audit_status=1, is_visible=1 WHERE audit_status=0;
  ```
  以及 stress_test.sh §2 添加 setup post force-visible 的临时改动（详见 `stress_issues_log.md` #S6）

---

## #C2 search 关键字 `verify` 命中应用层限流 → Phase 1 v3 78% non-2xx

- **发现于**：Phase 1 v3（2026-06-05 11:50）
- **症状**：search 场景 `GET /api/v1/search?q=verify` 在 10 RPS / 60s 下 461/591 = 78% non-2xx，P99 飙到 238ms（其他场景 P99 ≤ 18ms）
- **观察**：nginx 已 100000r/s（不可能是 nginx 限流），且其他场景同 10 RPS 0 错误 → 错误来源是 **app 层**
- **根因猜测**：
  - 可能是搜索模块自带的 query 限流（每关键字 N 次/秒）
  - 或者 ES/SQL 在 single-hot-key 反复命中下触发了 RateLimiter
  - 需要查 `internal/service/search/` 和 `pkg/errcode` 里 450XXX 段的码
- **影响**：search 在 Phase 2-7 都不能用「单一硬编码 keyword」打压
- **建议修复**（v2，**不在压测期内做**）：
  - 如果是产品决策的搜索限流（防爬虫），保留，但要在 PRD 里 explicit 记下来
  - 如果是无意引入的过严限流，放宽阈值
- **当前 workaround**：压测脚本侧改 `wrk_get_search.lua` 从关键字池里随机选（记在 `stress_issues_log.md` #S3）

---

## #C3 app 层硬编码 rate limit 阻断中等并发压测（200/day per user · 30/min per IP）

- **发现于**：Phase 2 r30 nginx log 分析（2026-06-05 12:30）
- **症状**：
  - like 场景 1145/1800 = 64% non-2xx：拼 nginx log → 全是 app 返回 429
  - search 场景 1770/1802 = 98%：全是 app 429（已知 #C2，本条把根因坐实）
- **根因**：`cmd/server/main.go` 路由装配里 4 个 endpoint 都套了 `middleware.RateLimit`，参数硬编码：
  | endpoint | KeyFunc | Limit | Window |
  |---|---|---|---|
  | `POST /api/v1/posts` | per user | 20 | 24h |
  | `POST /api/v1/posts/:id/like` | per user | 200 | 24h |
  | `GET /api/v1/search` | per IP | 30 | 60s |
  | `POST /api/v1/upload/presign` | per user | 30 | 1h |
- **影响**：
  - 压测期任何中等 RPS 都会大量 429，结果失真
  - 真实生产期：单用户连发 200 个 like 会被拦，可能产品体验不友好（点赞频率 < 1 次/7 min）
- **建议修复**（v2，**不在压测期内做**）：
  - 把硬编码 Limit / Window 挪到 `config.yaml ratelimit.*` 子段，prod / dev 各自调整
  - 保留默认值（不偏移现有行为），但增加可配置性
  - 压测期临时绕过：跑前调高这些常量重新构建镜像，或把限流 middleware 改成 "no-op when env=stress"
- **当前 workaround**：**不修，接受降级**。Phase 2-7 把 search / like 标为"已知降级场景"，看 P50/P99 不看 non-2xx 率

---

*—— 自动维护，发现新业务代码问题追加在末尾*
