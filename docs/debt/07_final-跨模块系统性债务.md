# Final · 跨模块系统性债务 + 收口

> **审计范围**：所有不归单模块、必须站在仓库全景才能看清的债务。每条 TD-SYS 都串起 Batch 1–6 已登记的多个子项，给出"一次性收口"的处置建议。
>
> **审计意图**：在 6 个 Batch 之后做一次"提级"，避免每个 Batch 内部讨论的局部问题被各自解决又各自不彻底；同时给 `09_三视图索引.md` 提供取舍依据——重构顺序应从这些跨模块债开始，而不是逐模块清单。
>
> **关联材料**：所有 Batch 1–6 文件、PRD v3.0、conventions、CLAUDE.md §七 / §八 / §十一

---

## 跨模块条目（TD-SYS-NN）

### TD-SYS-01 · Config-First 规则跨 7 个模块系统性破洞

- **档位**：1（违反 §九 评分卡 B4 / CLAUDE.md §七强制规则 1，跨模块重复）
- **代码锚点**：
  - `internal/cache/user_cache.go:incrAndCheck` 内 `24*time.Hour` 硬编码（TD-USER-03）
  - `internal/service/search_service.go` `maxKeywordLen=50` / `booleanOperators` 包级 const（TD-SEARCH-01）
  - `internal/handler/upload.go` `PresignReq.Size` 硬编码 `max=5242880`（TD-UPLOAD-02）
  - `internal/service/follow_service.go` `maxFollowing/defaultFollowListSize/maxFollowListSize` 三个 const（TD-FOLLOW-01）
  - `internal/middleware/cors.go:13` `Access-Control-Allow-Origin: *` 写死（TD-CORS-01）
  - `pkg/config/config.go` `AuditConfig` 缺 Timeout/MaxRetries 字段（TD-AUDIT-01）
  - `configs/config.prod.yaml` 缺 `consumer` / `cache` / `metrics` 三段（TD-INFRA-09）
  - `cmd/server/main.go:273` `30*time.Second` shutdown timeout 硬编码（见 TD-SYS-04）
- **现状**：体检 M1/M2 阶段把 ConsumerConfig / CacheConfig 收进 cfg，但只覆盖了"被体检报告点名的两处"。后续 Step 8/9/10/13 新写的代码、Step 12 收口的 cors / response，全部再次回到"包级 const + 文件内 const + 注释 TODO config"老路。**Config-First 不是个别违规，而是项目层面没有 lint / pre-commit 兜底**——一次违反易复刻，三次违反成习惯。
- **理想**：(1) 一次性收口 PR，把上述 7 处全部进 `pkg/config/config.go`；(2) `cfg.Validate()` 加 fail-closed 校验（零值即 fatal）；(3) 加 `scripts/lint_no_magic.sh`（grep 黑名单 + golangci-lint customchecks），CI 卡。否则 Step 18 起 prod 后这种破洞会再出 5 处。
- **触发条件**：Step 18 prod 启动时 TD-INFRA-09 直接触发；其余在 prod 调参时陆续触发。
- **重构成本**：M（收口 PR 1 天 + lint 接入 1 天 + cfg.Validate 半天）
- **关联 ADR**：§九 评分卡 B4 / CLAUDE.md §七 强制规则 1 / conventions §1.1
- **关联偏离点**：体检报告 M1/M2 已修但未泛化；progress/13–17 未补 Config-First 自查项
- **建议处置**：立即修（这是档 1 系统性违规，最该严判）

---

### TD-SYS-02 · 错误码段位 + HTTP status 双重一致性破洞

> **状态：✅ 已修复（Step 18 pre-cleanup A2）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：1（B1 段位规则 + §九 评分卡）
- **代码锚点**：
  - `pkg/errcode/errcode.go` 文件头注释"X=4 客户端 / X=5 服务端"与 470xxx（审核）/ 480xxx（手机号加密）矛盾（TD-ERRCODE-01）
  - `internal/handler/health.go:80` `code:503001` 越段，errcode 表无此号（TD-INFRA-08）
  - Post 模块 `412xxx` 与 PRD §7 标称 `420xxx` 不一致（TD-ERR-01）
  - `encryption` 错误码声称"never returned to HTTP"但 service 层实际回传（TD-ERR-02）
- **现状**：错误码段位有三处独立证据指向"段位规则定下来后没有人维护"。审核 / 加密占据 X=4 段，新写的 health 直接捏出 503001，连段位都没人 review；PRD 标称 420xxx 而代码实际 412xxx 一直保留。
- **理想**：(1) errcode.go 头注释改成实际段位表（删 X=4/X=5 二分规则，因为现实已不符）；(2) 503001 改 `ErrServiceUnavailable = New(503000, ..., 503)` 进 errcode.go；(3) PRD §7 表与代码 audit 一次性对齐；(4) errcode 单测：所有 `New(code, ..., http)` 都断言 code/100 ∈ {4,5} 且 http 同段。
- **触发条件**：监控告警分析时；前端 i18n 接错误码表时；面试官问段位策略时。
- **重构成本**：S（半天）
- **关联 ADR**：PRD §7 / §九 评分卡 B1
- **关联偏离点**：progress/12 偏离点提过收口但未彻底
- **建议处置**：立即修

---

### TD-SYS-03 · 跨包共享常量散落多份 / 字符串重复（drift 隐患）

- **档位**：3（跨模块妥协演变为漂移源）
- **代码锚点**：
  - `internal/middleware/request_id.go` 与 `pkg/response/response.go:13` 各定义一份 `requestIDKey = "X-Request-ID"`（TD-ERRCODE-02）
  - `internal/handler/health.go:83` 又一次 `c.GetString("X-Request-ID")` 裸字符串
  - `internal/handler/follow.go` / `post.go` / `upload.go` 重复 `parseUserIDParam` / `parsePathID` / `strconv.ParseInt` 三套（TD-FOLLOW-03 / TD-POST-09）
- **现状**：低层共享语义（request ID context key、path id 解析）没有 single source of truth，每个新模块拷一份近似实现，长期演化必出 drift（如有人改一处 key 大小写就静默断裂）。
- **理想**：(1) 建 `pkg/ginx/`（或 `pkg/ctxkey/`）放 `RequestIDKey` 常量 + `ParseIDParam(c, name) (int64, error)` helper；(2) 全仓库 grep 替换为同一引用；(3) 在 `08_护栏清单.md` 记入"不许在 handler 包内再写 ParseInt"。
- **触发条件**：有人改 key 大小写 / 顺序时；新模块加路径参数时（必复制旧 helper）。
- **重构成本**：S
- **关联 ADR**：§七 可复用资产规范
- **关联偏离点**：progress/8 / progress/9 / progress/12 偏离点已分别提及
- **建议处置**：Step 18 启动前修

---

### TD-SYS-04 · `main.go` 30s shutdown timeout 硬编码，且 ctx 未覆盖 ConsumerManager / OSS / Bus / Redis / MySQL

> **状态：✅ 已修复（Step 18 pre-cleanup A3）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：1（违反 Config-First B4 + 优雅停机不完整）
- **代码锚点**：
  - `cmd/server/main.go:273` `shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)`
  - `cmd/server/main.go:276` 仅 `srv.Shutdown(shutdownCtx)` 用了 shutdownCtx
  - `cmd/server/main.go:283-310` `consumerMgr.Shutdown()` / `ossClient.Close()` / `bus.Close()` / `rdb.Close()` / `sqlDB.Close()` 全部未受 shutdownCtx 限制
- **现状**：(a) 30 秒不在 cfg；(b) shutdownCtx 名字暗示"整个 shutdown 流的超时"，实际只罩 HTTP server。若 consumer drain 卡住（如 MySQL 慢查询），后续步骤会无限等待，K8s/Docker 的 stop timeout 直接 SIGKILL，DrainChan 设计精心保证的"不丢事件"被外层暴力打破。
- **理想**：(1) `cfg.Server.ShutdownTimeout time.Duration`，默认 30s；(2) 每阶段独立 budget（HTTP 5s / Consumer 15s / Bus 5s / DB 5s）；(3) 阶段超时时 `log.Warn` 提示数据可能丢失；(4) Consumer Shutdown 内部把 outer ctx 传入 DrainChan 的 deadline。
- **触发条件**：prod 滚动更新；K8s SIGKILL 时；MySQL 慢查询导致 consumer drain 阻塞时。**这是 GUARD-06 设计的"反向背刺"**——drain 子路径用 background ctx 是对的，但外层时限缺失。
- **重构成本**：M（要改 ConsumerManager.Shutdown 签名）
- **关联 ADR**：PRD §4.3 优雅停机 / GUARD-06
- **关联偏离点**：progress/13 偏离点已提"DrainChan 保证 in-flight"，但未提外层 budget
- **建议处置**：Step 18 启动前修

---

### TD-SYS-05 · TOCTOU 模式跨三模块复制粘贴

- **档位**：2（架构所限：业务读后写场景在 service 层做判断的固有问题）
- **代码锚点**：
  - `internal/service/like_service.go:Like` — SISMEMBER → AddLike 之间并发产生重复 publish（TD-LIKE-02）
  - `internal/service/follow_service.go:Follow` — Exists → CountFollowing → INSERT IGNORE 三段非原子，可越上限（TD-FOLLOW-02）
  - `internal/service/upload_service.go:Callback` — GETDEL 先于 ownership 校验，攻击者猜中 nonce 可烧掉受害者上传槽（TD-UPLOAD-03）
- **现状**：三处都是"先 read 决策，再 write 落库"，service 层做的"安全检查 + 业务写"分离。每处单看都是档 2/3，但**模式重复 3 次**说明项目缺乏一个标准的"原子语义层"。
- **理想**：(1) 关键业务点（like / follow / upload）都迁移到 Lua / 行锁 / DB unique constraint 兜底，service 层不做读后判定；(2) `08_护栏清单.md` 加一条"新增写路径必须先回答：TOCTOU 在哪一层兜底"；(3) Final 增加跨模块代码评审清单。
- **触发条件**：流量增长后高并发触发；安全审计追"重复 like / 越上限 follow / nonce 抢占"问题时。
- **重构成本**：L（三处独立改，每处都要 Lua + 兼容测试）
- **关联 ADR**：CLAUDE.md §七 强制规则 2（Lua 原子）
- **关联偏离点**：分布在 progress/05 / 08 / 09
- **建议处置**：触发时修；先把 follow 上限校验改 Lua（最危险，可被刷刷到 3001）

---

### TD-SYS-06 · 三个 observability admin 面零鉴权（/metrics / Grafana / RabbitMQ Mgmt）

> **状态：✅ 已修复（Step 18 pre-cleanup B1 + B3）→ /metrics 公网 404；Grafana 走 SSH tunnel 127.0.0.1:3000；RabbitMQ Mgmt prod 不 publish。详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：2（架构所限：dev 网络隔离当鉴权）
- **代码锚点**：
  - `cmd/server/main.go` Prometheus `/metrics` 直接挂主路由（TD-METRICS-02）
  - `docker-compose.yml:252` Grafana admin/admin（TD-METRICS-03）
  - `docker-compose.yml:204` RabbitMQ Management 端口 15672 暴露，凭据 cooking/cooking123（本条统一收）
- **现状**：三个不同模块独立暴露 admin 面，靠 Docker 网络隔离当鉴权。生产环境网络拓扑变化（同集群多租户 / VPC peering / 临时调试开公网）任一发生即裸奔。
- **理想**：admin 面统一走"独立端口 + basic auth + IP 白名单"三选一；至少 prod compose 内置 reverse proxy 鉴权一层。
- **触发条件**：进入 prod；或开发期临时暴露端口到公网调试。
- **重构成本**：M（三处分别接入鉴权 / 改 Prometheus 端配 + 改 Grafana auth provider）
- **关联 ADR**：PRD §10 / CLAUDE.md §九 防御深度
- **关联偏离点**：progress/16 已提
- **建议处置**：Step 18 起 prod 前

---

### TD-SYS-07 · prod / dev / docker 三套 yaml 字段对齐无 CI 校验

> **状态：✅ 已修复（Step 18 pre-cleanup A1）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：1（违反 Config-First B4 的衍生：配置漂移）
- **代码锚点**：
  - `configs/config.yaml`（dev）139 行
  - `configs/config.docker.yaml`（docker dev）109 行
  - `configs/config.prod.yaml`（prod 模板）109 行 — 缺 consumer/cache/metrics 三段
- **现状**：三套 yaml 没有"必须有同样 key 集"的机器校验，新加字段时容易漏 prod 模板（TD-INFRA-09 直接触发例）。
- **理想**：`scripts/check_config_parity.sh`：load 三套 yaml，diff 它们的 top-level + nested key 集合；CI 卡。任何字段差异必须显式 allowlist（如 dev 不需要 ratelimit 而 prod 必需，写进 allowlist）。
- **触发条件**：每次新加配置段时（已发生多次）。
- **重构成本**：S（半天写脚本 + 接 CI）
- **关联 ADR**：§七 Config-First
- **关联偏离点**：无登记
- **建议处置**：立即修

---

### TD-SYS-08 · `/health` `/readiness` 等监控端点的 response envelope 与业务接口不一致

> **状态：✅ 已修复（Step 18 pre-cleanup A2）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：1（违反 A5 handler 模板：bind → validate → service → response 四步）
- **代码锚点**：
  - `internal/handler/health.go:38` Health 用 `response.Success`（OK）
  - `internal/handler/health.go:79-84` Readiness 503 时直接 `c.JSON` 写裸 envelope（不一致）
- **现状**：监控端点错误路径绕过 `response.FromError`，与 TD-INFRA-08 是同一处症状但属于不同视角——"模板违规" vs "errcode 越段"。
- **理想**：response 包补 `response.Unavailable(c, data)` / `response.WithStatus(c, code, data)` 通用方法；所有 handler 强制经过 response 包。
- **触发条件**：前端按 `code` 字段路由展示 / 错误码统计 / 日志聚合查 `request_id`。
- **重构成本**：S
- **关联 ADR**：§九 A5
- **关联偏离点**：无
- **建议处置**：立即修（与 TD-SYS-02 / TD-INFRA-08 同一 PR）

---

### TD-SYS-09 · 13 个 `verify_step*.sh` 未接 CI，本地通过不等于 CI 通过

> **状态：✅ 部分修复（Step 18 pre-cleanup C2）→ verify-step17 已接入 pr.yml；其余 verify_step3..16 / golangci-lint 仍未做，留待 Step 18 之后立项。详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：4（MVP 妥协）
- **代码锚点**：
  - `scripts/verify_step5.sh` … `scripts/verify_step17.sh` 共 13 个文件
  - `.github/workflows/pr.yml`（仅 `go vet` + `staticcheck` + `go test`）
- **现状**：conventions §1 多次声明 verify 脚本是收口必跑，但 PR 提交时 CI 不会跑。Step 12 体检报告 H1 RabbitMQ 实现缺失就是被开发者本地 skip verify 偷渡到 main 的实例。
- **理想**：CI 加 `make verify-current-step`（按 PR 分支名 `feature/step-N-*` 取 N），或者 PR 描述里强制贴 verify 输出。
- **触发条件**：每次有人懒得本地跑 verify 时；常态。
- **重构成本**：M（要可靠提取 step 编号 + CI 跑得起 Docker Compose）
- **关联 ADR**：CLAUDE.md §七 / conventions §1
- **关联偏离点**：progress/17 偏离未登记
- **建议处置**：Step 18 启动前接最低限度（先接 verify_step17）

---

### TD-SYS-10 · Consumer 缺消息级 idempotency（PV / Count 在 RabbitMQ at-least-once 下漂移）

- **档位**：2（架构所限：当前 Event 信封无 ID 唯一性约束）
- **代码锚点**：
  - `internal/consumer/pv_consumer.go:flush` 直接 += deltas（TD-PV-01）
  - `internal/consumer/count_consumer.go:flush` 同（TD-COUNT-01）
  - `internal/event/types.go` Event 有 `ID` 字段但消费侧未用作去重
- **现状**：Like consumer 靠 `INSERT IGNORE + uk_user_post` 兜底（GUARD-12），但 PV / Count 走纯增量 UPDATE，RabbitMQ at-least-once 重投会让 view_count / following_count 持续偏高。**Step 13 切到 RabbitMQ 后债务从理论变现实**。
- **理想**：方案 A — Event ID 落 Redis SETEX（24h）做去重；方案 B — 改用"绝对值快照"事件而非"增量"事件，幂等天然成立；方案 C — PV/Count 表加 `last_event_id` 列，UPDATE 时 WHERE 卡。
- **触发条件**：Step 13 切 RabbitMQ 后；触发 redelivery（broker 重启 / channel 关闭 / 进程崩溃）时。
- **重构成本**：M（方案 A）/ L（方案 B 改事件 schema）
- **关联 ADR**：PRD §4.9 idempotency 设计；GUARD-12（like 已兜底）
- **关联偏离点**：progress/13 偏离登记 PV/Count 未做幂等
- **建议处置**：触发时修；切 RabbitMQ 上线第一周 dashboard 观察 view_count / following_count 漂移，确认是否破阈值再投入。

---

### TD-SYS-11 · 加密 / 审核 fail-open vs fail-closed 不一致

- **档位**：3（跨模块策略缺统一）
- **代码锚点**：
  - `pkg/crypto/phone.go:23` `EncryptPhone(plain, keyHex="")` 静默回落明文（TD-CRYPTO-01，fail-open）
  - `internal/service/user_service.go:GetMyProfile` decrypt 失败回空 phone_masked（TD-CRYPTO-03，fail-open）
  - `internal/consumer/audit_consumer.go` API 失败 → `audit_status=0 / is_visible=0` 永久（TD-AUDIT-02，fail-closed）
  - `internal/service/user_service.go:VerifyAccessToken` Redis 故障仅 Warn（GUARD-15，fail-open，已 deliberate）
- **现状**：四处合规 / 安全相关的故障路径选择不一致。同样是"底层依赖失败"，加密选明文落库（fail-open），审核选拒绝可见（fail-closed），JWT 黑名单选放过（fail-open）。**fail-open 的两处是 deliberate（GUARD-15）+ 隐性默认（TD-CRYPTO-01），项目层面没有"安全失败策略"决策表**。
- **理想**：在 `08_护栏清单.md` 加一张表 — 每个外部依赖失败时的 fail 方向 + 理由 + 限流降级方案；TD-CRYPTO-01 必须改 fail-closed（明文落库是真 P0），其余视场景重判。
- **触发条件**：合规审计；或 prod Redis/MySQL/Aliyun 任一抖动时实际观察故障行为。
- **重构成本**：S（决策表）+ M（TD-CRYPTO-01 改 fail-closed 含数据回扫）
- **关联 ADR**：GUARD-15 / CLAUDE.md §九
- **关联偏离点**：progress/11 / progress/12 分别登记
- **建议处置**：立即修 TD-CRYPTO-01；其余在决策表统一回审

---

### TD-SYS-12 · N+1 模式跨模块（读 N+1 + 写 N+1）

- **档位**：1（违反 A3 Repository 模板：批量场景必须批量查询）
- **代码锚点**：
  - `internal/service/post_service.go:AuthorAssembler.LoadMap` 顺序 N 次 FindByID（TD-POST-01，读 N+1）
  - `internal/repository/like_repository.go:IncrPostLikeCount/DecrPostLikeCount` for 循环逐 post UPDATE（TD-LIKE-04，写 N+1）
  - `internal/consumer/pv_consumer.go:flush` 同（TD-PV-02，写 N+1）
  - Search 路径同样辐射 AuthorAssembler N+1（TD-SEARCH-02）
- **现状**：读 N+1 一处、写 N+1 两处、外加搜索连带受害一处。每个 Repository 模板要求"批量场景必须批量查询"，但 Step 4/5/7 实现时都跳过了 FindByIDs / `UPDATE...CASE WHEN` 方案。流量上来后这是最先暴露的性能瓶颈。
- **理想**：(1) `UserRepository.FindByIDs(ids []int64) (map[int64]*User, error)` + 上层 AuthorAssembler 改批量；(2) `likeRepository.IncrBatch(deltas map[int64]int)` 用 `UPDATE posts SET like_count = CASE id WHEN ... END WHERE id IN (...)`；(3) PV 同。
- **触发条件**：单页 Feed > 20 帖时 N+1 显形；高峰 Consumer flush 时 DB QPS 飙高。
- **重构成本**：M（每个 Repository 加批量方法 + service 路径切换）
- **关联 ADR**：§九 评分卡 A3
- **关联偏离点**：progress/04 已自承认；其余未登记
- **建议处置**：Step 18 起 prod 前必须修读 N+1（用户感知）；写 N+1 看监控触发再修

---

## main.go 装配复查（系统性发现）

- **boot 顺序文档化得很好**：`cmd/server/main.go:1-29` 头注释列了 12 步顺序，LIFO shutdown 也明文；这是 deliberate 决策。
- **boot 阶段错误处理一致**：1–6 步任一失败 `os.Exit(1)`；6 步之后用 ConsumerManager 启动失败 ⇒ Fatal log；这套模式正确。
- **风险点 1 — 配置 Validate 缺位**：`cfg, err := config.Load()` 后没有 `cfg.Validate()`，所以 TD-INFRA-09 / TD-CFG-03 那种"prod 缺 consumer 段"在 boot 阶段不报错，直到第一次 consumer flush 才异常。修法见 TD-SYS-01。
- **风险点 2 — initEventBus 在 Provider="" 时静默 fallback channel**：`main.go:564` `case "channel", "":` 这是 deliberate（dev 默认 channel），但 prod 配置漂移成 `provider:""` 时会落到 ChannelBus 而不报错，**事件全在内存里随实例销毁**。建议 prod 模式（`server.mode=release`）+ 空 provider ⇒ fatal。
- **风险点 3 — Shutdown LIFO 顺序写得对，但每阶段没 budget**：见 TD-SYS-04。

---

## Final 收口批注

- **本批新增**：12 条系统性条目（TD-SYS-01…12）+ 3 条 main.go 复查发现
- **本批档位分布**：档 1 = 6 / 档 2 = 4 / 档 3 = 1 / 档 4 = 1 = 12 条
- **跨批密度统计（含本批）**：
  - **档 1（设计失误）累计约 27 条**：集中在 Config-First（B4）/ 错误码段位（B1）/ Lua 原子（B5）/ N+1（A3）四个评分卡条目；这四类合计 ~16 条，占 档 1 的 60%。
  - **档 2（架构所限）累计约 14 条**：分布广，但 TOCTOU / at-least-once 幂等 / fail-open 一致性 三组各 2–3 条。
  - **档 3（跨模块妥协）累计约 11 条**：审美 / 解耦 / 加密一致性 居多。
  - **档 4（MVP 妥协）累计约 24 条**：大量集中在 Batch 6（生产基础设施）。
  - **总计 ~76 条**（详细数与归档以 `09_三视图索引.md` 为准）。
- **关键发现**：
  1. **档 1 不是孤立违规，是"项目缺 lint / pre-commit / CI 兜底"的症状**。TD-SYS-01 / TD-SYS-07 解决后档 1 复发概率应大幅下降。
  2. **Step 13 切 RabbitMQ 是项目最大风险窗口**：TD-SYS-10（消息幂等）+ TD-SYS-05（TOCTOU）+ TD-LIKE-05（缩放精度）+ TD-EVT-04（错误处理 channel/rabbitmq 不一致）四条债同时显形，前两条建议在 Step 13 实际投产前修完。
  3. **Step 18 起 prod 的"必修四件套"**：TD-INFRA-01（prod compose）/ TD-INFRA-09（prod cfg 缺段）/ TD-SYS-02（503001 越段）/ TD-SYS-04（shutdown timeout）。其余可在 prod 跑起来后按监控触发。
- **下一步动作建议**（给用户）：
  - 用 `09_三视图索引.md` 选下一批重构顺序；建议第一批做"档 1 且 S 成本"的所有条目，作为热身 PR。
  - GUARD-16..20 已在 `08_护栏清单.md` 收口，进入只读护栏池。
  - 本文档 + Batch 1–6 = v1.0 台账；后续季度复审时增量更新。

---

## 全审计收口

- **全表档位分布**：见上方"跨批密度统计"段（最终数以 `09_三视图索引.md` §1 为准）
- **三张索引交付状态**：见 `09_三视图索引.md`
- **护栏清单定稿状态**：v0 15 条 + 本套审计追加 GUARD-16…20，共 20 条，见 `08_护栏清单.md`
- **遗留未决项**：
  - Batch 5 收口批注的算术（5+2+3+1=11 而非自记的 10）—— 已识别但不返工，以本 Final 跨批密度统计为准
  - Final 未涉及的细分：测试覆盖率实际值、benchmark 缺失、E2E 缺失——本台账定位是"债务清单"而非"测试策略"
- **下一步建议**：
  - 用户先 review `09_三视图索引.md` 的 "档 1 且 S 成本" 子集，选出首批热身 PR（建议 5–8 条）
  - Step 18 启动前必修 4 件套；其余按触发条件分批处置
  - 季度内不再增量审计，下次审计建议在 Step 20（部署上线完成）后

---

_Final 完成。整个 v1.0 技术债与设计权衡台账落幕。_
