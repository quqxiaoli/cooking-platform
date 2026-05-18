# Batch 4 · 关注（Step 8） + 上传（Step 9）

> **审计范围**：
> - **关注**：`internal/model/follow.go`、`internal/repository/follow_repository.go`、`internal/service/follow_service.go`、`internal/handler/follow.go`、`internal/model/dto/follow.go`、`migrations/000004_*`
> - **上传**：`internal/cache/upload_cache.go`、`internal/service/upload_service.go`、`internal/handler/upload.go`、`internal/model/dto/upload.go`、`pkg/oss/`（client.go / aliyun.go / mock.go / whitelist.go / doc.go）
>
> **审计意图**：上传模块涉及跨账户安全面（callback nonce、URL 白名单二次校验），是档 1 严判的重点；关注模块审 self-follow 防御 + 计数同步是否走 CountConsumer。
>
> **关联材料**：PRD v3.0 §7（440xxx / 460xxx）/ §11.2、progress/09 偏离点（PresignedURL 代替 STS、nonce GETDEL 设计）

---

## 本批条目

### TD-FOLLOW-01 · `maxFollowing / defaultFollowListSize / maxFollowListSize` 三个包级 const 违反 Config-First

- **档位**：1
- **代码锚点**：`internal/service/follow_service.go:69`（`const maxFollowing = 3000`）、`internal/service/follow_service.go:74-77`（`defaultFollowListSize = 20` / `maxFollowListSize = 50`）
- **现状**：3000 关注上限、列表默认 20、列表上限 50，全部以包级 `const` 落地。其中 3000 是产品策略层硬约束（PRD-Phase2 §8 F-F01 AC-5「防僵尸关注」），未来若运营需要调整门槛（例如对认证创作者开 1w），必须改代码 + 重新构建镜像。
- **理想**：进 `pkg/config/config.go` 的 `FollowConfig`：`MaxFollowing int`（默认 3000）、`DefaultListSize int`（默认 20）、`MaxListSize int`（默认 50）；service 持有 `cfg.Follow`，clampFollowListSize 改读 cfg。
- **触发条件**：永远成立，但生产真正咬人的时机是「运营首次提议给特定群体放开 3000 cap」——届时改代码的成本暴露。
- **重构成本**：S（新增配置结构体 + 一处构造函数注入 + 三处 const 替换；现有测试不依赖具体数值）。
- **关联 ADR**：CLAUDE.md §七 强制规则 B2「Config-First」 / `docs/conventions/01_步骤13-17开发规范.md §1.1`
- **关联偏离点**：与 Batch 1 / Batch 2 / Batch 3 反复出现的 Config-First 半破属同根（TD-COMMON-02 / TD-SEARCH-01 / TD-LIKE 类）。Step 13 体检 M1 修复像点赞批量参数那样全 cfg 化时遗漏了 follow 模块。
- **建议处置**：与 Batch 5 收口时一并把 Config-First 半破全部清账，作为「Step 13 体检 M1 补丁」一次性 PR。

---

### TD-FOLLOW-02 · 3000-cap 的 Exists → CountFollowing → INSERT IGNORE 三段非原子（TOCTOU 可越过上限 1 ~ N 次）

- **档位**：2
- **代码锚点**：`internal/service/follow_service.go:138-165`（Follow 流程步骤 3-5）
- **现状**：Follow 流程顺序为：① `Exists` 探测；② `CountFollowing` 全表 COUNT；③ `INSERT IGNORE`。三步之间无任何锁 / 事务 / 原子。同一用户在 cap=3000 处并发发起 2 个 follow 请求（不同 target），两个请求都会读到 cnt=2999，都通过 cap，最终 follow 数=3001。N 个并发请求最多可越过 cap 同等的 N-1。
- **理想**：把 cap 校验下沉到 DB 层，用一条 INSERT-with-SELECT 原子化：`INSERT IGNORE INTO follows SELECT ?, ? FROM dual WHERE (SELECT COUNT(*) FROM follows WHERE follower_id=?) < ?`。或者引入 Redis Lua 把 cnt + 写入做成单原子操作（Lua 持久化前需先把 follows 计数同步到 Redis，复杂度抬升）。MVP 阶段可接受 ±N 的越界。
- **触发条件**：单用户并发关注请求 ≥ 2 + 当前 following 数恰好接近 3000。MVP 用户行为很少触发；自动化脚本批量关注、或公链/僵尸账号刷关注时可见。
- **重构成本**：M（DB 方案需要一条精心构造的 SQL + 一轮 EXPLAIN 验证；Lua 方案需要建立 follows 计数 Redis 镜像）。
- **关联 ADR**：PRD-Phase2 §8 F-F01 AC-5「防僵尸关注」
- **关联偏离点**：与 TD-LIKE-02（SISMEMBER → AddLike TOCTOU）同模式——这是项目「热路径前置校验 + 写入分离」反复出现的同类债。
- **建议处置**：档 2 永久接受 + 监控告警兜底：上线后埋一条 `following_count > maxFollowing` 的定时审计 SQL（每日跑一次），出现即触发 P3 工单人工 reconcile。等用户量 MAU > 50w 再考虑原子化改造。

---

### TD-FOLLOW-03 · 路径参数解析 helper（`parseUserIDParam` / `parsePostID` / `parsePathID`）散布同根

- **档位**：3
- **代码锚点**：`internal/handler/follow.go:141-148`（`parseUserIDParam`）、`internal/handler/like.go`（`parsePostID`）、`internal/handler/post.go`（`parsePathID`）
- **现状**：四套 handler 各自实现一份「parse path int64 → 写错误响应 → 返回 ok bool」的 helper。签名、错误码、内联实现完全一致，但分散在三个文件里；新模块再加 helper 时几乎肯定再复制一份。
- **理想**：抽到 `internal/handler/helpers.go`（或 `internal/middleware/path_param.go`）的 `parsePathInt64(c, key) (int64, bool)` 单实现，全模块复用。
- **触发条件**：永远成立。生产不直接咬人，但每加一个模块就累积一次复制成本，重构 PR 的 diff 会越拉越长。
- **重构成本**：S（一次性新增 helper 文件 + 全仓 grep 替换 ≤ 4 处）。
- **关联 ADR**：CLAUDE.md §七 「模板归口」原则（handler 层模板：bind → validate → service → response）
- **关联偏离点**：与 Batch 2 TD-POST-09 同源——首席工程师视角的同一笔债，分散登记是因为每个 batch 只能看见自己模块的副本。Final 收口时合并为单条系统性债。
- **建议处置**：与 Final 收口时统一打包 `internal/handler/helpers.go` 抽取重构 PR。

---

### TD-FOLLOW-04 · `CountFollowing` 走 `COUNT(*)` 全表扫，未利用 `users.following_count` 冗余字段

- **档位**：4
- **代码锚点**：`internal/repository/follow_repository.go:197-207`（`CountFollowing` 方法）+ `internal/service/follow_service.go:148-153`（每次 Follow 调用）
- **现状**：每次 Follow 请求都要 `SELECT COUNT(*) FROM follows WHERE follower_id=?`。对 uk_follower_following 走 follower_id 前缀，扫描行数等于该用户 following 数；接近 3000 cap 的活跃用户每次 follow 都吃满扫描。`users.following_count` 字段已由 CountConsumer 维护，但**故意不被 cap 校验复用**——因 CountConsumer 是 eventually-consistent，硬 cap 校验必须基于源真表。
- **理想**：保留 follows 表的精确 COUNT 作为 cap 兜底，但「软 cap 预检」用 `users.following_count` 先挡掉绝大多数请求（cnt + safety_margin < cap 直接放行不再 COUNT）。仅在 `cnt 接近 cap` 时落到精确 COUNT。
- **触发条件**：单用户接近 3000 cap 时高频 follow（产品上罕见）；或活跃用户海量并发 follow 触发 follows 表热点（更罕见）。
- **重构成本**：S（service 层加一段 fast-path：先 FindByID 拿 users.following_count，距离 cap > 100 直接放行；否则走 COUNT 精确路径）。
- **关联 ADR**：PRD-Phase3 §5.2 / follow_service.go 文件头「Why follows are written synchronously, but counts asynchronously」
- **关联偏离点**：故事线 08 自陈「Counted from the source-of-truth table, not the lagging users.following_count」是刻意决策，但**仅在 cap 校验场景**刻意；未在 fast-path 场景刻意取舍。
- **建议处置**：观察生产慢 SQL 监控；若 follow_repository.CountFollowing 进 P99 慢查 top-N 再启动 fast-path 改造。可永久接受到 MAU > 50w。

---

### TD-FOLLOW-05 · `ListFollowing` 在 `uk_follower_following` 上的 keyset 列序非完美覆盖（自陈）

- **档位**：2
- **代码锚点**：`internal/repository/follow_repository.go:218-220`（`ListFollowing`）+ `migrations/000004_create_follows_table.up.sql:30-33`（自陈注释）
- **现状**：ListFollowing 走 `WHERE follower_id=? AND f.id<? ORDER BY f.id DESC`，uk_follower_following 索引列序为 (follower_id, following_id)，f.id 不在索引内。Migration 文件头自陈「其 keyset 列序非完美覆盖，但有 3000 关注上限兜底（单用户扫描集 ≤ 3000 行），MVP 完全可接受」。这是档 2 的典型样态——架构层面承认次优、有明确兜底、决策有文档。
- **理想**：新增 `idx_follower_id_id (follower_id, id)` 让 keyset 完美覆盖。但代价是多一份索引体积 / 写放大；且**单用户 3000 cap 已封顶扫描集**，二级索引投资回报率低。
- **触发条件**：3000-cap 被放宽（见 TD-FOLLOW-01 触发条件）→ 单用户扫描集突破 1w → keyset 在生产慢 SQL 监控里冒头。
- **重构成本**：S（追加一条 migration 加索引；前提是先观测到慢 SQL，否则盲加索引违反 YAGNI）。
- **关联 ADR**：migrations/000004 文件头第 4 条注释
- **关联偏离点**：本条是 Migration 自陈，登记到债务台账只为 Final 三视图索引可见；架构决策本身无争议。
- **建议处置**：档 2 永久接受 + 触发条件中加一句「3000 cap 放宽时配套加索引」，等 TD-FOLLOW-01 真触发时一并落地。

---

### TD-UPLOAD-01 · `oss.IsAllowedURL` 用 `strings.HasPrefix` 防御深度依赖配置带 trailing slash，但 `config.validate` 不强制

- **档位**：3
- **代码锚点**：`pkg/oss/whitelist.go:30-39`（`IsAllowedURL`）+ `pkg/config/config.go:405-442`（OSS validation 段，强制 URLPrefix 小写但未强制 trailing slash）
- **现状**：白名单实现为 `strings.HasPrefix(strings.ToLower(u), strings.ToLower(prefix))`。当 `cfg.OSS.URLPrefix` 配置成 `https://oss.example.com/bucket`（无尾 `/`），攻击者持 `https://oss.example.com/bucket-evil/...` 这种共前缀但实际属于不同 bucket / 路径的 URL，可绕过白名单。三套 yaml（dev / docker / prod）现状都带尾 `/` 兜底，但**没有任何代码层保证**——下一个填配置的人写错就破防。注释自陈「callers must therefore configure URLPrefix in lower case — config.validate enforces this」，trailing slash 同等关键却未 enforce。
- **理想**：在 `pkg/config/config.go` 的 OSS validation 段补一句 `if !strings.HasSuffix(cfg.URLPrefix, "/") { return fmt.Errorf("oss.url_prefix must end with '/'") }`。或更严格：在 `IsAllowedURL` 内部统一兜底，进 prefix 时若无尾 `/` 主动追加。配置层 enforce 更"显形"，调试更友好。
- **触发条件**：永远存在；真触发要求「填配置者忘了 trailing slash」+「攻击者发现并构造共前缀 URL」。后者依赖 OSS bucket 命名风格被泄漏。
- **重构成本**：S（4 ~ 6 行代码，validate 加一条断言；现有 yaml 全部合规，加 enforce 不破坏现状）。
- **关联 ADR**：CLAUDE.md §九「防御深度」/ PRD v3.0 §11.2
- **关联偏离点**：与 GUARD-11（avatar_url 二次白名单）是同一防御深度链条的不同环节——上层（IsAllowedURL）破防则下层都失效。
- **建议处置**：本批落地为新增护栏候选 GUARD-16：「OSS URLPrefix 配置必须以 `/` 结尾」，由 `config.validate` 强制；下次重构清单清账时同步实现。

---

### TD-UPLOAD-02 · `PresignReq.Size` 在 DTO 硬编码 `max=5242880` (5 MiB) 与 `cfg.OSS.MaxImageSize` 双源

- **档位**：1
- **代码锚点**：`internal/model/dto/upload.go:35`（`Size int64 \`binding:"required,min=1,max=5242880"\``）+ `internal/service/upload_service.go:71-75`（service 层二次校验）
- **现状**：DTO binding 写死 5 MiB，service 层再读 `cfg.OSS.MaxImageSize` 二次校验，依靠 service 二次校验「掩盖」DTO 写死的事实。注释自陈「Service layer re-checks against cfg.OSS.MaxImageSize so env overrides take effect without touching the wire format」——但**只能让 cfg < 5 MiB 时生效**，cfg > 5 MiB 时 DTO 先拒，二次校验不可达。Config-First 半破。
- **理想**：DTO 去掉 `max=5242880`，只保留 `min=1`；service 层校验维持。或保留 DTO 写死的硬上限作为"安全网"，但要把它升到一个永远不会被业务上限触达的值（如 100 MiB），让 cfg 真正成为业务上限的唯一来源。
- **触发条件**：运营提出"高质量封面图允许 8 MiB"——这时改 cfg.OSS.MaxImageSize=8388608 后端依旧 400，必须同步改 DTO 重编译，违背 Config-First 初衷。
- **重构成本**：S（删一个 binding tag + 同步 swagger / API doc 上限描述）。
- **关联 ADR**：CLAUDE.md §七 B2 Config-First / `docs/conventions/01_步骤13-17开发规范.md §1.1`
- **关联偏离点**：与 TD-FOLLOW-01 / TD-SEARCH-01 同根 Config-First 半破。
- **建议处置**：与 TD-FOLLOW-01 / TD-SEARCH-01 合并为「Config-First 补丁」一次性 PR；同时把 DTO 文档注释里「5 MiB DTO ceiling」改成「DTO 留 100 MiB 安全网，业务上限以 cfg.OSS.MaxImageSize 为准」。

---

### TD-UPLOAD-03 · Callback 先 GETDEL 再校验 user_id ownership —— 攻击者猜中 nonce 即可烧掉受害者的上传槽

- **档位**：3
- **代码锚点**：`internal/service/upload_service.go:143-160`（Callback 流程顺序）
- **现状**：`ConsumeNonce` 先 GETDEL 原子消费 nonce，**之后**才比较 `rec.UserID != userID`。当 ownership mismatch 时，nonce 已被删除——受害者再调 Callback 会收到 460104（"nonce 无效"），原始上传作废。攻击者需要：① 登录任意账号；② 在 15min TTL 内猜中受害者的 32 字符 hex nonce。猜中难度为 2^-128（实务上不可达），但「先消费再校验」的代码顺序是设计层面的反模式——nonce 校验失败不应有副作用。
- **理想**：把 ownership 检查下推到 Lua 脚本，与 GETDEL 一起原子：`local v = redis.call('GET', KEYS[1]); if v == false then return nil; end; local r = cjson.decode(v); if r.user_id ~= tonumber(ARGV[1]) then return 'mismatch'; end; redis.call('DEL', KEYS[1]); return v`。或简单点：先 GET 校验，再 DEL（两次调用但读写分离，mismatch 时 nonce 不被消耗）——这放弃了 GETDEL 的双 callback 原子性，但 nonce 重复消费可由 `INSERT IGNORE` 兜底（如果 callback 写库存有唯一索引）。
- **触发条件**：理论上永远成立，实际触发要求 128 位猜中——可视为永不触发的设计层面瑕疵；但作为 PRD §11.2 安全文档的可信度，仍值得修。
- **重构成本**：M（Lua 方案需在 upload_cache 新增一个原子脚本 + 单测；简单 GET-then-DEL 方案需重新论证 GUARD-10 GETDEL 原子性放弃的影响）。
- **关联 ADR**：PRD v3.0 §11.2 nonce 设计 / GUARD-10
- **关联偏离点**：progress/09 自陈「callback 只接 nonce，server 自行查所有元信息；GETDEL 一次性原子消费杜绝重放」——刻意决策但只考虑了"重放"轴，未考虑"恶意提前消费"轴。
- **建议处置**：档 3 触发时修；2026 H2 安全复审时一并打包 Lua 化（与 TD-LIKE-01 / TD-LIKE-03 的 Lua 收口同 PR）。新增护栏候选 GUARD-17（与 Batch 3 GUARD-17 搜索 OFFSET 候选编号冲突，本批改用 GUARD-18）。

---

### TD-UPLOAD-04 · Mock OSS handler 不验证 nonce / signature，仅校验 size

- **档位**：4
- **代码锚点**：`pkg/oss/mock.go`（`handlePUT` 实现）+ `pkg/oss/mock.go`（embedded HTTP listener on `cfg.MockListenAddr`）
- **现状**：dev 环境 MockClient 嵌入 HTTP listener 接收 PUT。handler 只校验 Content-Length 不超过 cfg.OSS.MaxImageSize，**不验证 SignURL 签名是否有效、不验证 nonce 是否对应**。这意味着 dev 环境任何客户端只要拿到 mock 监听地址，都能伪造 PUT 写入。
- **理想**：mock 实现一个 minimal 签名校验（HMAC-SHA1 / SHA256 with `cfg.OSS.Mock.SecretKey`）和过期时间校验，让 dev 环境也能跑 e2e 签名失败用例（如签名错、签名过期、双方时钟漂移）。
- **触发条件**：dev 环境永远触发；prod 不触发（prod 切 AliyunClient，签名由阿里云 OSS 服务端校验）。所以本质是「测试覆盖不足」债。
- **重构成本**：M（实现 HMAC 签名校验 + 在 service 层 / 集成测试中触发签名错路径 ≥ 2 用例）。
- **关联 ADR**：PRD v3.0 §11.2 / CLAUDE.md §七 "三件套接口 + Mock + Real"（GUARD-09）
- **关联偏离点**：故事线 09 未明确登记此偏离——Mock 实现简化时未对照"Mock 行为应近似 Real 的所有失败模式"原则审视。
- **建议处置**：档 4 标准处置——Step N 重估时纳入。MVP 期间不动；上线前安全复审 / 渗透测试覆盖 OSS 路径时按真服务路径走，不依赖 Mock 验证。

---

### TD-UPLOAD-05 · `ConsumeNonce` 把 corrupt JSON 静默吞为 `ErrCacheNotFound`，丢失观测信号

- **档位**：3
- **代码锚点**：`internal/cache/upload_cache.go:93-99`（`ConsumeNonce` 内 `json.Unmarshal` 失败分支）
- **现状**：`json.Unmarshal` 失败时直接返回 `nil, ErrCacheNotFound`，理由是「Corrupt record — surface as not-found rather than 500. The caller experience is the same: their nonce is no longer usable」。但 corrupt JSON 在 Redis 里只可能由「外部直接写入」或「序列化 bug」造成，是必须告警的运维事件。当前实现把这种事件降级成「用户看到 460104」，运维侧看不到任何日志/指标信号。
- **理想**：在 `json.Unmarshal` 失败分支补一行 `zap.L().Error("upload nonce corrupt", zap.String("nonce", nonce), zap.Error(err))`，并 increment 一个 `upload_nonce_corrupt_total` 计数器（Step 16 Prometheus 接入后）。用户响应仍然返回 ErrCacheNotFound 兼容，但运维有信号。
- **触发条件**：永远成立，但触发频率非常低（要求 Redis 持久化文件被外部改、或本应用代码有 NonceRecord schema 变更导致旧数据不可解析）。
- **重构成本**：S（3 ~ 5 行代码）。
- **关联 ADR**：CLAUDE.md §九「防御深度」+ Step 16 Prometheus 监控对齐
- **关联偏离点**：与 Batch 3 TD-SEARCH-05（搜索零结果不可观测）同根「故意降级，未保留运维信号」。
- **建议处置**：与 Batch 3 TD-SEARCH-05 合并为「Observability 信号补齐」一次性 PR；Step 16 Prometheus 接入收口时同步落地。

---

## 本批批注（收口时填写）

- **档位分布**：档 1 = 2（TD-FOLLOW-01 / TD-UPLOAD-02） / 档 2 = 2（TD-FOLLOW-02 / TD-FOLLOW-05） / 档 3 = 4（TD-FOLLOW-03 / TD-UPLOAD-01 / TD-UPLOAD-03 / TD-UPLOAD-05） / 档 4 = 2（TD-FOLLOW-04 / TD-UPLOAD-04） = 10
- **关键发现**：
  1. **白名单防御深度脆弱点**：`oss.IsAllowedURL` 的 `strings.HasPrefix` 实现安全性强依赖 `cfg.OSS.URLPrefix` 带尾 `/`，但 `config.validate` 强制小写却不强制 trailing slash——这是项目目前最容易在「换一个 OSS bucket / 改一次 yaml」时静默破防的一处。建议立刻把 GUARD-16 写进护栏并落实 config 校验。
  2. **Config-First 全局清账时机已到**：Batch 2 TD-USER 段、Batch 3 TD-SEARCH-01 / TD-LIKE 段、本批 TD-FOLLOW-01 / TD-UPLOAD-02 共计 ≥ 5 处包级 const / DTO 硬编码违反 B2 规则。建议 Batch 5 / Final 之间专门打包一次性 PR 清账，比逐 batch 改成本低。
  3. **3000-cap TOCTOU 不是孤例**：TD-FOLLOW-02 与 Batch 3 TD-LIKE-02 / TD-COUNT-01 同模式——「热路径前置校验 + 写入分离」的并发漏洞在四个模块（点赞、计数、关注、上传）都出现，但每一处的修复路径不同（DB unique index / Lua / 原子 SQL / nonce 设计）。这是一类系统性债，建议 Final 跨模块章节专门起一段 `TD-SYS-TOCTOU` 总结四处共性。
- **新增护栏**：
  - **GUARD-16 候选**：`cfg.OSS.URLPrefix` 配置必须以 `/` 结尾，由 `config.validate` 强制（配合 TD-UPLOAD-01）。本批先候选，Final 阶段定稿。
- **下一批衔接点**：
  - **Batch 5（审核 Step 10 + 加密 Step 11 + Step 12 错误码/脱敏收口）**：审 `audit_service` 的 fail-closed 实现是否真的在所有失败模式（HTTP 5xx / timeout / 鉴权失败）都 fail-closed；审 AES-GCM 手机号加密的 key 轮换路径（PRD §11.1 隐含的"如何换 pepper"未在代码中可见）；审错误码全局重复 / 缺失（pkg/errcode/errcode.go 全表 grep）。
  - **本批遗留**：TD-UPLOAD-03 中提到的 GUARD 编号冲突需要在 Final 阶段对齐（搜索 OFFSET 候选 vs nonce ownership 原子化）。
