# Batch 3 · 点赞（Step 5） + 搜索（Step 7）

> **审计范围**：
> - **点赞**：`internal/model/like.go`、`internal/repository/like_repository.go`、`internal/cache/like_cache.go`（含 `addLikeScript` / `decrClampScript`）、`internal/service/like_service.go`、`internal/handler/like.go`、`internal/consumer/like_consumer.go`、`internal/consumer/pv_consumer.go`、`internal/consumer/count_consumer.go`、`internal/consumer/helper.go`（DrainChan）、`internal/model/dto/like.go`、`migrations/000003_*`
> - **搜索**：`internal/repository/search_repository.go`、`internal/service/search_service.go`、`internal/handler/search.go`、`internal/model/dto/search.go`
>
> **审计意图**：异步消费 + Lua 原子是本仓库最复杂的并发面，必须逐行核 A2/B5 规则。搜索模块审 MySQL FULLTEXT 的可演进性（PRD §12 已标注 P2 债）。
>
> **关联材料**：PRD v3.0 §4 / §6.2、conventions §1.2 / §1.3、体检 §1.1 / §3.1 / §4.2 (M3) / §6 (M4)

---

## 本批条目

<!--
条目模板（复制使用）：

### TD-LIKE-NN / TD-SEARCH-NN · <一句话标题>

- **档位**：N
- **代码锚点**：`path/to/file.go:FuncName:LINE`
- **现状**：
- **理想**：
- **触发条件**：
- **重构成本**：S / M / L
- **关联 ADR**：
- **关联偏离点**：
- **建议处置**：
-->

### TD-LIKE-01 · `LikeCache.RemoveLike` 仅 DECR 走 Lua，SREM+EXPIRE 仍走 Pipeline，与 AddLike 不对称

- **档位**：1（违反 B3 / §七 强制规则 2：Redis 读后写 / mutate-mutate 配对必须用 Lua）
- **代码锚点**：`internal/cache/like_cache.go:RemoveLike:183-213`（SREM+EXPIRE 用 `c.rdb.Pipeline()`；decrClampScript 单独 Eval）；对照 `addLikeScript:132-143`（SADD+EXPIRE+条件 INCR 全在一个 Lua）
- **现状**：Step 13 体检 M3 修了 AddLike → 单一 Lua 原子；RemoveLike **只 Lua 化了 DECR**，SREM 留在 Pipeline。两步之间窗口仍在：SREM 成功 → Redis 抖动 → DECR Lua 未跑，set 成员已减但 count 未减，需要 7d sliding TTL 失效或人工清理才能恢复一致。
- **理想**：单一 `removeLikeScript`：KEYS=[set, cnt]，ARGV=[user, ttl]；当且仅当 `SREM` 返回 1 时执行带 0-clamp 的 DECR；与 AddLike 对称。
- **触发条件**：滚动重启、Redis Sentinel failover、Lua 加载失败重试间隔 → 同一 (user, post) 出现 "已在 set 但 cnt 偏低"。
- **重构成本**：S（< 1h，合并两个脚本 + 单元测试）
- **关联 ADR**：体检 M3 / GUARD-07
- **关联偏离点**：体检 M3 修复时 AddLike / RemoveLike 仅修了前者；偏离点未登记
- **建议处置**：**立即修**，合并为 `removeLikeScript`。与 TD-LIKE-03 同 PR。

### TD-LIKE-02 · `LikeService.Like` TOCTOU：SISMEMBER → AddLike 之间并发产生重复 publish

- **档位**：2（架构所限：Service 决策 publish 的判断点早于 Lua 真正下手；当前接口契约无法零成本修）
- **代码锚点**：`internal/service/like_service.go:Like:107-141`（SISMEMBER 在 service 内、AddLike 是另一次 Redis 调用、publish 决策只看 `already` bool）
- **现状**：两个并发 Like 请求 A、B 都过 SISMEMBER（均返回 false）→ A 的 Lua SADD=1 + INCR；B 的 Lua SADD=0 + INCR 被跳过。但 **Service 看不到 B 的 Lua 跳过**，B 仍 `publishLikeEvent`。结果：Redis count 正确，likes 表 INSERT IGNORE 也正确（GUARD-12 兜底），**但 CountConsumer 没有消息级去重**（见 TD-COUNT-01）→ `users.total_likes` +2 而非 +1。
- **理想**：`addLikeScript` 显式返回 `(was_new int, count int)` 二元组；Service 只在 `was_new==1` 时 publish。或退一步：把 SISMEMBER 也并进同一脚本，由 Lua 做 "幂等探测 + 写 + 通报" 三合一。
- **触发条件**：Channel 模式单实例 + 单用户连续双击罕见但可触发；切 RabbitMQ at-least-once + 双实例后稳定可触发。
- **重构成本**：S（脚本改返回 + service 判 was_new）/ M（含 CountConsumer 端补丁，与 TD-COUNT-01 联动）
- **关联 ADR**：PRD §4.9 idempotency / GUARD-12（uk_user_post 兜 likes 表，但**不兜 users 计数**）
- **关联偏离点**：本质是 PRD §4.9 idempotency 未下沉到 publish 决策这一层
- **建议处置**：**切 RabbitMQ 前（Step 13 当前重审）必修**；与 TD-COUNT-01 同方案族。

### TD-LIKE-03 · `like_cache.go` 文件头 "SADD-and-INCR 非原子" 段在 Step 13 Lua 化后过时未更新（文档腐烂）

- **档位**：1（违反 B5：注释 / PRD / 代码一致性；与 Batch 1 TD-ERR-01 错误码段位文档腐烂同源）
- **代码锚点**：`internal/cache/like_cache.go:40-43`（"We accept the small inconsistency window between SADD-and-INCR (and SREM-and-DECR)..."）；矛盾事实：`addLikeScript` 已在 Step 13 让 SADD+INCR 原子化
- **现状**：注释仍把 LikeConsumer 标榜为"补救 Redis 不一致的 source of truth"；新人 reviewer 读完会以为 Step 13 没修。注释与代码的真实状态背离。
- **理想**：注释更新为："SADD+INCR 已通过 `addLikeScript` 原子化；SREM+DECR 当前仍为 Pipeline+Lua 两步（待 TD-LIKE-01 修复）；LikeConsumer 仍作为 MySQL 最终一致来源，但 Redis 一致性窗口已大幅收窄。"
- **触发条件**：永不触发功能问题；触发认知误判。
- **重构成本**：S（< 5 min）
- **关联 ADR**：体检 M3 修复决策 / B5 文档一致性
- **关联偏离点**：体检 M3 修代码漏更注释
- **建议处置**：**立即修**；与 TD-LIKE-01 同 PR。

### TD-LIKE-04 · `likeRepository.Incr/DecrPostLikeCount` 用 for 循环逐 post UPDATE，N+1 写

- **档位**：4（MVP 妥协，自承认 / 与 PV、Count 三处同病）
- **代码锚点**：`internal/repository/like_repository.go:IncrPostLikeCount:195-213`、`DecrPostLikeCount:216-232`；自承认注释 `internal/repository/like_repository.go:186-194`（"CASE WHEN 方案放弃，loop 简单"）
- **现状**：单批 50 events ≈ 50 distinct posts → 50 条 UPDATE round-trip。MVP 注释承认 "MySQL handles this trivially"，但叠加 PVConsumer 同病 (TD-PV-02) + CountConsumer 同病时 MySQL master QPS 会先于业务量翻番。
- **理想**：单条 `UPDATE posts SET like_count = like_count + CASE id WHEN 1 THEN ? WHEN 2 THEN ? ... END WHERE id IN (?,?,...)`；或 batch 临时表 + 一条 JOIN UPDATE。
- **触发条件**：用户量起来后 LikeConsumer flush 高峰；slow log 报警；与 GUARD-13 读写分离矛盾（更多 UPDATE 全压在 master）。
- **重构成本**：M（含基准 + 慢日志验证）
- **关联 ADR**：自承认 / GUARD-13 读写分离
- **关联偏离点**：无
- **建议处置**：MAU 10w 阈值前评估；与 PVConsumer / CountConsumer 一并改造（共享 "批量 UPDATE 工具" helper）。

### TD-LIKE-05 · `scaleDeltas` 比例缩放在 RabbitMQ at-least-once 下精度滑动，自承认无补偿任务

- **档位**：4（MVP 妥协，自承认 / GUARD-12 部分兜底）
- **代码锚点**：`internal/consumer/like_consumer.go:scaleDeltas:336-382`；自承认注释 `internal/consumer/like_consumer.go:30-51`（"Exact per-post correction is left to a future verify-against-SELECT COUNT(*) FROM likes GROUP BY reconcile job (post-MVP)"）
- **现状**：当 duplicates 在 post 间分布不均时，按 ratio 分配的 like_count 漂移可见。Channel 模式下 RowsAffected==len(rawLikes) 几乎恒等，自带保险；切 RabbitMQ 后这道保险失效。
- **理想**：消费端先按 `(user_id, post_id)` 去重再 BatchInsert + 直接拿 perPost 真实 count；或定时 reconcile 任务 `UPDATE posts SET like_count = (SELECT COUNT(*) FROM likes WHERE post_id = id)`。
- **触发条件**：Step 13 + 高并发热帖（同帖 5s 内不同用户重复点赞 + 部分重投）。
- **重构成本**：M（去重 + 任务调度框架）
- **关联 ADR**：自承认 / GUARD-12
- **关联偏离点**：无
- **建议处置**：保留 + Step 18 后加 reconcile cron。

### TD-LIKE-06 · `LikeService.Like` 幂等命中路径仍 `postRepo.FindByID`，一次多余 MySQL 读

- **档位**：4（MVP 微优化）
- **代码锚点**：`internal/service/like_service.go:Like:108-129`（FindByID 先于 SISMEMBER 调，但 SISMEMBER=true 时 AuthorID 这次没用上）
- **现状**：每个 "用户重复点同一帖" 都触发一次无用 MySQL FindByID。重复点赞在 UI 容易被双击/网络重试触发。
- **理想**：把 FindByID 推迟到 "决定要 publish 之后"；或把 post.UserID 也缓存到 Redis（`like:author:{post_id}` TTL 7d，与 like:set 同生命周期）。
- **触发条件**：MAU 起来后 master FindByID QPS 偏高，slow log 上易见 hot post 重复 SELECT。
- **重构成本**：S（推迟）/ S+（缓存方案）
- **关联 ADR**：无
- **关联偏离点**：无
- **建议处置**：低优先；与 TD-LIKE-04 同 PR 打包优化。

### TD-COUNT-01 · `CountConsumer` 无消息级 idempotency，RabbitMQ at-least-once 后用户计数漂移

- **档位**：2（架构所限：与 PRD §4.9 idempotency 设计有结构性矛盾，**Step 13 切 RabbitMQ 前的最关键债**）
- **代码锚点**：`internal/consumer/count_consumer.go:flush:315-384`（每条事件直接 +=/-=，无 event_id 去重）；对照 `internal/consumer/like_consumer.go:flushWithCtx:262-324` 用 INSERT IGNORE 兜底（GUARD-12）
- **现状**：LikeConsumer 用 INSERT IGNORE + uk_user_post 兜住 likes 表；CountConsumer 直接累加 `users.total_likes / follower_count / following_count / post_count`，**完全没有消息去重**。RabbitMQ at-least-once 重投一条 LikeEvent → users.total_likes +2 而非 +1。重启前接到的 ACK 在网络抖动后会重投，是常态而非异常。
- **理想**：(a) Redis `SETNX consumer:processed:{event_id}` TTL 24h 做消费端去重；(b) 或派生事件方案：LikeConsumer 把 "实际新增 (user_id, post_id) 对" 二次 publish，CountConsumer 订阅派生事件复用 INSERT IGNORE 兜底（事件树更复杂但 throughput 友好）。
- **触发条件**：**Step 13 RabbitMQ 切换后稳定可触发**；任何 ACK 超时、网络抖动、消费者重启都会触发重投。
- **重构成本**：M（方案 a）/ L（方案 b）
- **关联 ADR**：PRD §4.9 idempotency / GUARD-12 / 体检 H1
- **关联偏离点**：progress/13 RabbitMQ 切换若未补 idempotency，本条是隐性偏离
- **建议处置**：**切 RabbitMQ at-least-once 上线前必修**；优先方案 (a)，pkg/eventdedupe 复用给 PVConsumer / CountConsumer。

### TD-COUNT-02 · `CountConsumer` 五 topic 共享单 flushLoop，单 topic 高峰饿死其他 topic 的 ticker

- **档位**：2（架构所限：共享 channel + 单 flush loop 是设计选择）
- **代码锚点**：`internal/consumer/count_consumer.go:Start:127-208`（5 个 Subscribe goroutine 共享 eventCh + 单一 flushLoop:226-292）；自陈 file header `internal/consumer/count_consumer.go:53-59`（"co-locating saves connection-pool pressure"）
- **现状**：batchSize 由 "所有 topic 之和" 决定。商业化后 TopicLike 速率 >> TopicFollow，TopicLike 反复填满 batch 触发 cap-flush，**TopicFollow 的少量更新一直被夹带 flush**——好处是低延迟；坏处是若 TopicLike 异常停滞（消费者饿死），TopicFollow 的 follower_count 同步会延迟到 10s ticker 才动。
- **理想**：要么各 topic 独立 consumer（与 PRD §3.1 EventBus 接口分离的精神一致）；要么 channel 按 topic 分桶 + 加权调度。
- **触发条件**：TopicLike 异常处理慢（DB 死锁 / slow query）→ TopicFollow 的 follower_count 延迟拉长 → 个人页 follower 数 "不动"。
- **重构成本**：M（拆分）/ L（加权调度）
- **关联 ADR**：file header 自陈 / PRD §3.4 / GUARD-01 接口分离
- **关联偏离点**：无（PRD 未明示 "必须合并 vs 拆分"）
- **建议处置**：触发后再拆；MVP 接受。

### TD-PV-01 · `PVConsumer` 无消息级 idempotency，RabbitMQ at-least-once 后 view_count 持续偏高

- **档位**：2（与 TD-COUNT-01 同源结构性债）
- **代码锚点**：`internal/consumer/pv_consumer.go:flush:162-185`（每条 PVEvent 累加进 deltas map，直接 `view_count += delta`）
- **现状**：PostService.GetDetail 上游 Redis SETNX `pv:{post}:{user}` 只防 "同用户同帖短窗重复"，**防不了同一 PVEvent 被 MQ 重投**。重投 → view_count 偏高 → 推荐 / 排序 / 运营报表全部偏。
- **理想**：与 TD-COUNT-01 同方案：`pkg/eventdedupe` 提供 `SETNX consumer:processed:{event_id}` 一体化能力。
- **触发条件**：Step 13 切 RabbitMQ + 任何重投。
- **重构成本**：S（复用 TD-COUNT-01 的基础设施）
- **关联 ADR**：PRD §4.9
- **关联偏离点**：同上
- **建议处置**：与 TD-COUNT-01 一次性修复（同基础设施一次性接入三个 consumer）。

### TD-PV-02 · `PVConsumer.flush` 单 SQL 单帖 UPDATE，与 TD-LIKE-04 同病

- **档位**：4（MVP 妥协）
- **代码锚点**：`internal/consumer/pv_consumer.go:flush:167-184`
- **现状**：`for postID, delta := range deltas { db.Exec("UPDATE posts SET view_count = view_count + ? WHERE id = ?") }`；100 events × 50 distinct posts → 50 UPDATEs / 5s。PRD §3.4 注释 "100/5s" 没有提及 N+1 写代价。
- **理想**：CASE WHEN 单 SQL；或与 TD-LIKE-04 / CountConsumer 共享 "批量计数累加" helper。
- **触发条件**：MAU 起后日 PV 千万级，view_count UPDATE 占 master QPS 显著比例。
- **重构成本**：M
- **关联 ADR**：自承认 / GUARD-13
- **关联偏离点**：无
- **建议处置**：与 TD-LIKE-04 / CountConsumer 一并改造（一个 PR 三个 consumer）。

### TD-SEARCH-01 · `maxKeywordLen=50` / `booleanOperators` 包级常量，违反 Config-First (B2)

> **状态：✅ 已修复（Step 18 pre-cleanup A5）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：1（违反 §七 强制规则 1 Config-First；体检 M2 已修 Consumer/Cache 但**漏修 Search**）
- **代码锚点**：`internal/service/search_service.go:maxKeywordLen:57` / `booleanOperators:62`
- **现状**：搜索关键词长度上限、BOOLEAN MODE 运算符黑名单两个调参参数硬编码为包级 const。PM 想在商业化阶段对中英文做差异化（中文 30 字 / 英文 80 字）→ 必须改代码 + 重新构建镜像。
- **理想**：迁到 `cfg.Search.MaxKeywordLen` + `cfg.Search.BooleanOperators`；NewSearchService 接收 `config.SearchConfig`。
- **触发条件**：商业化阶段 PM 提需求 → 走完整 CI 流水线，与体检 M2 修复理由完全一样。
- **重构成本**：S（< 1h）
- **关联 ADR**：CLAUDE.md §七 / conventions §1.1 / 体检 M2
- **关联偏离点**：体检 M2 修复时遗漏 search 模块
- **建议处置**：**立即修**；与 §七 Config-First 全表 audit 联动。

### TD-SEARCH-02 · 搜索路径吃同一个 AuthorAssembler N+1（与 TD-POST-01 同源，跨模块共享受害）

- **档位**：1（违反 A2 Repository 模板：缺 `FindByIDs`；本条登记 search 路径辐射）
- **代码锚点**：`internal/service/search_service.go:Search:130`（`assembler.LoadMap`）；自承认 `internal/service/author_assembler.go:77`（"add userRepo.FindByIDs and swap it in here; every caller benefits at once"）
- **现状**：search 单页 50 帖 + 热门关键词命中分散作者 → 单次 search 内最多 50 次 `userRepo.FindByID` round-trip；与 Batch 2 TD-POST-01 共享 assembler 实例，**一改两收益**。
- **理想**：实现 `UserRepository.FindByIDs(ctx, ids []int64) ([]*User, error)` 一条 IN 查询；LoadMap 改造为单批查 + map 映射。Feed + search 同时受益。
- **触发条件**：用户量起来后 search 路径 P95 与 feed 一同被 N+1 拖累。
- **重构成本**：S（FindByIDs 实现）+ S（LoadMap 改造）= S
- **关联 ADR**：CLAUDE.md §七 Repository 模板 / author_assembler.go:77 自承认
- **关联偏离点**：TD-POST-01 已登记；本条登记 search 路径
- **建议处置**：**与 TD-POST-01 同 PR 修复**，辐射 ≥ 2 模块。

### TD-SEARCH-03 · `SearchRepository` 同一 keyword 参数化传两次，依赖 MySQL planner 复用 FULLTEXT score 计算

- **档位**：3（跨模块妥协：GORM API 与 MySQL planner 之间的接缝）
- **代码锚点**：`internal/repository/search_repository.go:SearchByTitle:118-138`（Select 一次 + Where 一次，靠 `const matchExpr` 字面量相同让 planner 复用 score）
- **现状**：注释自陈 "MySQL recognises the repeated MATCH(...) AGAINST(...) and computes the relevance score a single time per row"。一旦 const 被人改 / GORM 升级重写 SELECT 字段顺序 / MySQL 升级 planner 行为变化 → score 计算翻倍，性能腰斩，难以从 EXPLAIN 上一眼看出。
- **理想**：(a) GORM Raw SQL 自拼，把 matchExpr 写成显式 alias 后 ORDER BY 复用；(b) derived table + JOIN 让 planner 显式只算一次。
- **触发条件**：GORM 大版本升级或 MySQL 升级时性能回归；slow log 报警。
- **重构成本**：S
- **关联 ADR**：file header §"Index behaviour"
- **关联偏离点**：无
- **建议处置**：低优先；下次 search 性能改造或 GORM/MySQL 升级前一并处理。

### TD-SEARCH-04 · `SearchService.Search` Debug 日志包含 keyword 明文，未脱敏

- **档位**：3（合规边界 / 跨模块妥协：与 Step 12 日志脱敏专项作用域不一致）
- **代码锚点**：`internal/service/search_service.go:Search:141-147`（`zap.L().Debug("search executed", zap.String("keyword", safe), ...)`）
- **现状**：Debug log 输出 `safe`（sanitised keyword）。Debug 在 prod 默认关闭，但 **dev/staging 全开**；ELK 抽样到 dev 流量时关键词会进运营 / 开发面板。用户搜 "人名 / 手机号 / 病症 / 公司"等敏感词即泄漏。
- **理想**：(a) Debug 改 `zap.Int("keyword_len", len([]rune(safe)))` 而非明文；(b) 仅 trace level 输出 keyword 且 trace 永不上 prod。
- **触发条件**：合规审计 / 内部红队抽样发现关键词流出。
- **重构成本**：S
- **关联 ADR**：CLAUDE.md §十三 / Step 12 日志脱敏决策
- **关联偏离点**：Step 12 脱敏仅覆盖 user/post 字段（phone / email / IP），**未覆盖 search keyword**
- **建议处置**：**立即修**；与 Batch 5 加密 / 脱敏审计一并查 search keyword + filter 参数全链路是否还有泄漏点。

### TD-SEARCH-05 · 搜索 resp 缺 `total` 字段、无 metrics、零结果无观测

- **档位**：4（MVP 妥协；Step 16 监控接入后会被立刻发现）
- **代码锚点**：`internal/service/search_service.go:Search:135-139`（resp 仅 Posts/NextCursor/HasMore）；`internal/handler/search.go:43-56`（无 metrics 埋点）
- **现状**：(1) 用户看不到 "共 N 条"，"无结果" 与 "全部失败" 在前端表现一致；(2) 无 `search_request_total{scene}` / `search_zero_result_total` / `search_duration_seconds`，运营提 "零结果率" 时 ELK grep 难答。
- **理想**：(a) metrics：Step 16 监控接入时一并补；(b) total：COUNT(*) WHERE MATCH 二次查询代价可接受（FULLTEXT 已建索引），但 P95 会涨 — 评估再说。
- **触发条件**：Step 16 监控接入；运营首次提零结果率需求。
- **重构成本**：S（metrics）/ M（total）
- **关联 ADR**：PRD §7 F-S01
- **关联偏离点**：无
- **建议处置**：metrics 部分 Step 16 必修；total 触发时再说。

---

## 本批批注（收口时填写）

- **档位分布**：档 1 = 4（TD-LIKE-01 / TD-LIKE-03 / TD-SEARCH-01 / TD-SEARCH-02）/ 档 2 = 4（TD-LIKE-02 / TD-COUNT-01 / TD-COUNT-02 / TD-PV-01）/ 档 3 = 2（TD-SEARCH-03 / TD-SEARCH-04）/ 档 4 = 5（TD-LIKE-04 / TD-LIKE-05 / TD-LIKE-06 / TD-PV-02 / TD-SEARCH-05）；合计 15 条
- **关键发现**：
  1. **Step 13 RabbitMQ 切换前最大风险簇**：TD-COUNT-01 + TD-PV-01 + TD-LIKE-02 三条共同指向 "下游 consumer 缺消息级 idempotency"。GUARD-12（uk_user_post）只兜了 likes 表本身，**没兜 users 计数表、posts.view_count、CountConsumer 全部 5 个 topic**。切 RabbitMQ at-least-once 后用户可见数字会偷偷漂移。建议在 Step 13 收口时同步落地 `pkg/eventdedupe`。
  2. **体检 M3 Lua 化收口不彻底**：TD-LIKE-01（RemoveLike SREM 还在 Pipeline）+ TD-LIKE-03（注释文档过时）= 同一个修复决策的两个未尽事宜。规则 B3 在 like 模块内部本身就半破窗，与 Batch 2 TD-USER-02（incrAndCheck 未走 Lua）形成 "B3 全仓库执行强度参差" 的整体图景。
  3. **批量 UPDATE 写 N+1 三处同病**：TD-LIKE-04 + TD-PV-02 + 未列名的 CountConsumer 第二段（按用户分 UPDATE）都是 "for ... db.Exec UPDATE" 模式。若 MAU 起来后这三处一起咬人，单 PR 抽 helper 一次解三处债。
  4. **跨 batch 共享受害模式确认**：TD-SEARCH-02 与 Batch 2 TD-POST-01 同根；FindByIDs 是 "一改双收益" 的高 ROI 改造，应优先于其他 S 级修复。
- **新增护栏**：本批正式追加 1 条候选护栏待 Final 阶段评议：
  - **GUARD-17 候选** · 搜索路径**故意走 OFFSET/LIMIT** 而非 keyset 游标（违反 PRD §5.4 默认 keyset 约定）。
    - 锚点：`internal/repository/search_repository.go:21-39` file header；`internal/service/search_service.go:parseSearchCursor:191-203`
    - 为什么是 deliberate 决策：FULLTEXT relevance 不是单调稳定列，(relevance, id) tuple 不能做安全 keyset；search 实际访问模式是前 1-3 页，OFFSET 深翻代价无关紧要。
    - 误删后果：未来重构者按 PRD §5.4 强行改 keyset → 跨页跳行 / 重复 → 用户体验事故。
- **下一批衔接点**：
  - **Batch 4（关注 + 上传）**：复核 `FollowConsumer`（已经合并进 CountConsumer 这条线，但 follow_service 自身可能仍有独立路径）；复核 upload 模块的限流 / Redis 用法是否复刻 user_cache 的 B3 漏洞。
  - **Batch 5（审核 + 加密 + 收口）**：与 TD-SEARCH-04 联动 — 加密专项时同步扫一遍 search keyword 链路；audit consumer 与 CountConsumer 同构（无 idempotency）情况复核。
  - **Batch 6（基础设施 Step 13-17）**：本批 4 条档 2 中至少 3 条（TD-COUNT-01 / TD-PV-01 / TD-LIKE-02）需要在 Batch 6 RabbitMQ 验证脚本中被显式断言（payload 重投实验）。
