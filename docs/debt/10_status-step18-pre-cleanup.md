# Step 18 启动前债务清理 · 已修复台账

> **生成时间**：2026-05-18
> **关联 commit**：`d753546 chore(step-18-pre): 启动前债务清理（A/B/C 三组 15 条）`
> **关联分支**：`feature/step-18-pre-debt-cleanup`
> **关联进度**：`docs/progress/18_项目进度追踪.md`
> **关联故事线**：`docs/storylines/18_故事线-第18步-预备-债务清理.md`
> **关联变更清单**：`docs/changes/18_代码变更清单-第18步-预备-债务清理.md`

---

## §0 文件定位

01–07 各 batch 是**审计快照**（生成于 2026-05-17，记录当时状态）。
09 三视图索引是**优先级地图**（用于决定清理顺序）。
本文件是**已修复结算账本**——只追加，不修改既往审计内容。

后续重构 PR 落地后，请新增 `docs/debt/11_status-stepNN-*.md`，
不要把状态写进 batch 文件正文里（保持快照不变）。

---

## §1 清理范围一览

本次冲刺动手项 **15 条**（A/B/C 三组），对应 TD 编号 **23 条**：

| 组别 | 动手项数 | 关联 TD 数 | 落地说明 |
|------|----------|------------|----------|
| A 组 P0 阻塞 | 7 | 13 + 多 TD 共修 | 含 SYS 类跨模块债 4 条；A7 关闭 TD-EVT-04 |
| B 组 prod 启动门面 | 3 | 6 | 含 INFRA / METRICS 类 |
| C 组 CI / Ops | 3 | 4 | 含 CI / SYS-09（🟡 部分）/ INFRA-04 |
| **合计** | **15** | **23**（去重后；3 条 🟡 部分） | — |

部分动手项一刀切多条 TD（例如 A1 同时关闭 INFRA-09 + SYS-07，A2 同时关闭
INFRA-08 + SYS-02 + SYS-08），所以"动手 15 条 / 清账 23 条"不矛盾。

---

## §2 按档位分

### 2.1 档 1 · 设计失误（13 条；TD-AUDIT-01 为 🟡 部分修复）

| 编号 | 一句话 | 落地动作 | 锚点修复后位置 |
|------|--------|----------|---------------|
| TD-EVT-04 | Handler 错误处理 channel vs rabbitmq 不一致 | A7：`event/bus.go` Handler 注释加 3 列对照表（行为差异显式契约化）；channel/rabbitmq 交叉引用 | `internal/event/bus.go` Handler 注释 |
| TD-INFRA-08 | health.go 503 越段 + 绕 envelope | A2：handler 改 `response.Unavailable`；errcode 头注释纠正 X 段位真相 | `internal/handler/health.go:78` + `pkg/errcode/errcode.go` 头 |
| TD-INFRA-09 | config.prod.yaml 缺 consumer/cache/metrics 三段 | A1：补齐 7 段 + `scripts/check_config_parity.sh` + Makefile target | `configs/config.prod.yaml` |
| TD-CRYPTO-01 | EncryptPhone(keyHex="") 静默 fallback 明文 | A4：改 fail-closed 返 `ErrEmptyKey`；errcode 新增 480103；release 模式 cfg.Validate 拒空 key | `pkg/crypto/phone.go:38/74` + `pkg/config/config.go` Validate |
| TD-USER-03 | incrAndCheck 内 24h 硬编码 | A5：提进 `cfg.Cache.UserSMSDailyTTL`；`NewUserCache` 加 TTL 参数 | `internal/cache/user_cache.go` 构造 |
| TD-SEARCH-01 | maxKeywordLen / booleanOperators 包级 const | A5：提进 `cfg.Search`；构造函数注入 | `internal/service/search_service.go` |
| TD-FOLLOW-01 | maxFollowing / list size 三 const | A5：提进 `cfg.Follow`；`clampFollowListSize` 提为方法；int↔int64 显式转 | `internal/service/follow_service.go` |
| TD-CORS-01 | CORS `*` 硬编码无 cfg | A5：`CORS(cfg.CORSConfig)`；origin map O(1) 查；`Vary: Origin`；release+"*" cfg.Validate 拒启动 | `internal/middleware/cors.go` |
| 🟡 TD-AUDIT-01 | AuditConfig 缺 Timeout / MaxRetries | A5：加字段 + cfg.Validate + 三 yaml 段（**调用方暂未接入，登记偏离，详见 §3**） | `pkg/config/config.go` AuditConfig |
| TD-SYS-02 | 错误码段位 + HTTP status 双重一致性破洞 | A2：errcode 头注释纠正"HTTPStatus 才是单一权威"；503001 别名 ErrServiceUnavailable | `pkg/errcode/errcode.go` 头 |
| TD-SYS-04 | shutdown timeout 硬编码 + ctx 不覆盖 | A3：三阶段 LIFO（5s/15s/5s），父预算来自 `cfg.Server.ShutdownTimeout`；`ConsumerManager.Shutdown(ctx)`；`closeWithDeadline` helper | `cmd/server/main.go:272-340` |
| TD-SYS-07 | 三套 yaml 字段对齐无 CI 校验 | A1：`scripts/check_config_parity.sh` + Makefile target；三 yaml 顶级 key 集合一致 | `scripts/check_config_parity.sh` |
| TD-SYS-08 | health 监控端点 envelope 不一致 | A2：与 TD-INFRA-08 同口子修 | `internal/handler/health.go:78` |

### 2.2 档 2 · 架构所限（2 条）✅

| 编号 | 一句话 | 落地动作 | 锚点修复后位置 |
|------|--------|----------|---------------|
| TD-METRICS-02 | /metrics 端口无鉴权 | B3：Nginx 公网 `location = /metrics { return 404; }`；内部 Docker 网络 scrape 不受影响 | `deploy/nginx/nginx.conf:87-89` |
| TD-SYS-06 | 三个 observability admin 面零鉴权 | B3 + B1：Grafana 走 SSH tunnel（compose `127.0.0.1:3000:3000`）；RabbitMQ 管理界面 prod 不 publish；/metrics 公网 404 | `docker-compose.prod.yml` + `deploy/nginx/nginx.conf` |

### 2.3 档 4 · MVP 妥协（8 条）✅

| 编号 | 一句话 | 落地动作 | 锚点修复后位置 |
|------|--------|----------|---------------|
| TD-INFRA-01 | 缺 docker-compose.prod.yml | B1：新增 281 行 prod compose | `docker-compose.prod.yml` |
| TD-INFRA-02 | 部署口令硬编码无 env_file | B1：全 secret 走 `${VAR:?required}`；新增 `.env.prod.example` 模板 | `docker-compose.prod.yml` + `.env.prod.example` |
| TD-INFRA-04 | slave init 无幂等保护 | C3：脚本顶部 `SHOW REPLICA STATUS` 探测，IO=Yes && SQL=Yes 直接 exit 0 | `deploy/mysql/init-slave.sh:55-72` |
| TD-INFRA-06 | Nginx 入口零防护 | B2：`client_max_body_size 6m` + `limit_req_zone api_zone 30r/s` + `sms_zone 1r/m` + `limit_conn_zone`；4 location 分段限流 | `deploy/nginx/nginx.conf:22-167` |
| TD-METRICS-03 | Grafana admin/admin | B1：Grafana `GF_SECURITY_ADMIN_PASSWORD` 强制走 `.env.prod` | `docker-compose.prod.yml` grafana service |
| TD-CI-01 | staticcheck@latest 无版本锁 | C1：锁到 `@2025.1.1` 附升级评审注释 | `.github/workflows/pr.yml:50-54` |
| TD-CI-02 | CI 未接 verify_step*.sh | C2：pr.yml staticcheck 后插 `make verify-step17` 自检 | `.github/workflows/pr.yml:59-60` |
| TD-SYS-09 | verify_step*.sh 未接 CI | C2：与 TD-CI-02 同口子修 | `.github/workflows/pr.yml:59-60` |

---

## §3 部分落地条目（需 Step 18 之后继续跟进）

### TD-AUDIT-01 · 字段已加，调用方未接

- **当前**：`AuditConfig.Timeout / MaxRetries` 字段已加，`cfg.Validate` 已卡空值，三 yaml 段位齐全。
- **未接**：`pkg/audit/aliyun.go` 的 `Review(ctx, ...)` 仍未把 `cfg.Audit.Timeout` 包装成 `context.WithTimeout`；`MaxRetries` 还没有重试 wrapper。
- **登记原因**：本步范围是"债务清理"，避免动 prod 行为；预留字段不会被野调用，cfg.Validate 在 release 模式校 Timeout > 0。
- **跟进时机**：Step 18 接入 prod 流量后第一周，根据 metrics（`audit_call_latency_seconds`）实测 P99 决定 Timeout 值，再合入调用方代码。

### TD-PV-01 / TD-COUNT-01 / TD-SYS-10 · idempotency 已加，但仍需观察

- **当前**：A6 新增 `internal/cache/event_dedup_cache.go`，PV/Count 在 subscribe 边界做 `SeenOrMark`；
  TTL 24h 覆盖 RabbitMQ requeue / DLX retry 窗口。
- **保留观察**：实测 RabbitMQ at-least-once 下漂移是否真的清零；Redis 故障 fail-open
  导致的偶发 double-count 是否能被 reconcile job 补回（reconcile job 还没写）。
- **跟进时机**：Step 18 上线后第一周看 `view_count` / 各 `*_count` 与 source-of-truth
  对账；若漂移可控，本组在三视图索引里可以从"档 2 触发时修"降为"永久接受"。
- **注**：这三条本次**没有正式标 ✅**，仍挂在档 2，需要 prod 数据验证。

---

## §4 与 09 三视图索引的关系

09 §4.1 列了"优先重构 Top 5"，本步全部命中：

| 09 §4.1 优先项 | 本步覆盖 |
|----------------|----------|
| 1. TD-INFRA-09 + TD-SYS-07 | ✅ A1 |
| 2. TD-CRYPTO-01 | ✅ A4 |
| 3. TD-INFRA-08 + TD-SYS-02 + TD-SYS-08 | ✅ A2 |
| 4. TD-SYS-04 | ✅ A3 |
| 5. Config-First 补丁 PR | ✅ A5（USER-03/SEARCH-01/FOLLOW-01/CORS-01/AUDIT-01 + INFRA-09 + SYS-04 同步处理） |

09 §4.2 列了"Step 18 prod 必修四件套"，全部命中：
TD-INFRA-01 ✅ / TD-INFRA-09 ✅ / TD-INFRA-08 ✅ / TD-SYS-04 ✅。

09 §4.2 列了"共享 helper 提取 PR"（TD-ERRCODE-02 / TD-FOLLOW-03 / TD-POST-09 /
TD-SYS-03）—— **本步未处理**，留待 Step 18 之后再立项。

---

## §5 剩余债务概览（不属于本次范围）

按 09 §1 总数 ~76 条扣除本次 23 条，剩余约 53 条。其中：

- **档 1 剩余约 15 条**：错误码 412xxx / 422xxx / N+1 / map 字段白名单 / log sanitize 等，
  Step 18 后立项专项 PR。
- **档 2 剩余约 11 条**：Refresh Token / TOCTOU / SDK ctx 不消费等，触发时修。
- **档 3 剩余约 11 条**：fail-open vs fail-closed 不对称、共享常量散落等，
  下次重构窗口动。
- **档 4 剩余约 16 条**：监控告警 AlertManager、SBOM、漏扫、Feed 游标精化等，
  按 Step 计划逐步消化。

下次复审建议：**Step 20 公网验证 + 压测**完成后，根据实测重新校准档位
（部分档 2 / 档 3 在压测后可能上升为档 1）。

---

## §6 维护约定

1. 本文件**只追加，不修改**——已 ✅ 条目不要回滚状态，需要 reopen 时写
   新文件 `12_reopen-*.md` 说明原因。
2. 各 batch 文件（01–07）正文**保持快照不变**，仅在条目标题下补一行
   `> **状态：✅ 已修复（Step 18 pre-cleanup）→ 详见 10_status-step18-pre-cleanup.md**`
   作为回链锚点。
3. 09 三视图索引的表格里给已修条目编号前加 ✅，"建议处置"列改为 `✅ 已修`，
   保留原行不删除（方便看历史优先级判断对不对）。
4. 后续重构 PR 命名规范：`docs/debt/1N_status-stepNN-<theme>.md`，N 单调递增。
