# Batch 2 · 用户模块（Step 3） + 内容模块（Step 4）

> **审计范围**：
> - **用户模块**：`internal/model/user.go`、`internal/repository/user_repository.go`、`internal/cache/user_cache.go`、`internal/service/user_service.go`、`internal/handler/user.go`、`internal/model/dto/user.go`、`pkg/jwt/jwt.go`、`cmd/migrate-phone/main.go`（手机号加密迁移工具的入口在此审）
> - **内容/Feed 模块**：`internal/model/post.go`、`internal/model/post_step.go`、`internal/model/scene_tag.go`、`internal/repository/post_repository.go`、`internal/cache/feed_cache.go`、`internal/service/post_service.go`、`internal/service/author_assembler.go`、`internal/handler/post.go`、`internal/handler/feed.go`、`internal/model/dto/post.go`、`migrations/000001_*` + `000002_*` + `000005_*`
>
> **审计意图**：MVP 第一波业务模块，是模板套用度的试金石。
>
> **关联材料**：PRD v3.0 §1 / §5.1 / §11.3、体检 §5.4

---

## 本批条目

### TD-USER-01 · 用户信息缓存接口（SaveUserInfo / GetUserInfo / Marshal / Unmarshal）为死代码

- **档位**：1（违反 A1 Cache 模板：Cache 接口必须有 Service 调用方；TD 评分卡 A1）
- **代码锚点**：`internal/cache/user_cache.go:SaveUserInfo:198` / `GetUserInfo:206` / `MarshalUserInfo:231` / `UnmarshalUserInfo:236`；调用方搜索仅 `DeleteUserInfo` 被 `internal/service/user_service.go:UpdateProfile:335` 使用
- **现状**：UserCache 写好了完整的 user-info 读 / 写 / 序列化 / 反序列化四个方法 + `keyUserInfo` builder + 30 min TTL 设计；UpdateProfile 也勤勉地 DeleteUserInfo（cache invalidate）—— 但全仓库 **没有任何代码 SaveUserInfo / GetUserInfo**。Auth 链路（`middleware/auth.go:Auth` → `user_service.go:VerifyAccessToken`）只做 JWT parse + blacklist + ban，没读用户记录；handlers（`GetMyProfile` / `GetPublicProfile`）直接走 `repo.FindByID`，每请求 1 次 MySQL。
- **理想**：要么删（YAGNI），要么在 Auth/Profile 链路真正接入：FindByID 前先 GetUserInfo，miss 时 SaveUserInfo 兜底，从 cfg.Cache 注入 TTL（目前注释里写死 30 min 也违反 Config-First）。
- **触发条件**：MAU 上来后 /users/me + /users/:id 是最热端点之一；DB 连接池压力 + P99 上涨；现在 DeleteUserInfo 调用空跑 Redis（无 key 时 DEL 仍正常返回，纯浪费一次 round trip）。
- **重构成本**：S（接入），M（删除 + 清理 DeleteUserInfo + PRD 文档同步）
- **关联 ADR**：CLAUDE.md §七 Cache 模板 / PRD-Phase2 §3 F-U04（"头像上传后 5 秒内全站更新"原引 DeleteUserInfo 作为依据）
- **关联偏离点**：progress/09 提到过 DeleteUserInfo，但 Save/Get 配套从未启用
- **建议处置**：**立即修文档**（PRD-Phase2 §3 F-U04 移除 DeleteUserInfo 描述），实现上倾向先**删**未使用的 Save/Get/Marshal/Unmarshal（YAGNI，减小攻击面 + 移除"看起来在用其实没用"的认知陷阱）；如需缓存再走 Lua 单写脚本统一实现。

### TD-USER-02 · SMS 限流 `incrAndCheck` INCR + EXPIRE 两段非原子，违反 §七 强制规则 2「Lua 原子操作」

- **档位**：1（违反 B3：Redis 读后写 / 条件写必须用 Lua）
- **代码锚点**：`internal/cache/user_cache.go:incrAndCheck:124-144`（注释自承认竞态："worst case is the counter never expires"）
- **现状**：INCR → if count==1 → EXPIRE 两条命令分两次 round trip。文件头注释承认"两个请求同时到 fresh key 时都可能在 EXPIRE 之前 INCR"，结论是"接受最坏 24h+ 延期"。但 `addLikeScript / decrClampScript` 已经走 Lua（参考 `internal/cache/like_cache.go`）—— 本模块未对齐。
- **理想**：写 Lua 脚本 `incrAndCheckScript`：INCR 后 if returned==1 then call("EXPIRE", KEYS[1], ARGV[1])；返回 count。一行 EVALSHA，原子 + 单 round trip。
- **触发条件**：高并发 send-code 时 daily counter 可能"永不过期"，第二天起所有请求被拒；恶意攻击者可以借此把某号码/IP "永久封"。
- **重构成本**：S（半天，含单测）
- **关联 ADR**：conventions §1.3 / 体检 M3 / GUARD-07（Lua 原子）
- **关联偏离点**：体检后修了 like_cache 的 Lua，user_cache 漏改
- **建议处置**：本批后小 PR 修；与 TD-USER-03 合并一次提交。

### TD-USER-03 · IncrementAndCheckSMSPhoneDaily / IncrementAndCheckSMSIPDaily 内部 TTL 硬编码 `24*time.Hour`

- **档位**：1（违反 B2：无包级常量 / Config-First）
- **代码锚点**：`internal/cache/user_cache.go:IncrementAndCheckSMSPhoneDaily:114`（`return c.incrAndCheck(..., 24*time.Hour)`）/ `IncrementAndCheckSMSIPDaily:121`（同）
- **现状**：daily 限流的 TTL（24h）由 cache 层硬编码。`pkg/config/config.go` 的 `RatelimitConfig` 已有 `SMSPerPhonePerDay/SMSPerIPPerDay`，但 daily-window 长度本身没有进 config —— 想压测改 12h 窗口需改代码。
- **理想**：进 `RatelimitConfig.SMSDailyWindow time.Duration`，default=24h。
- **触发条件**：运营临时活动想放松限流；与 TD-USER-02 一起 Lua 化后正好统一传 ttl 参数。
- **重构成本**：S（< 1h）
- **关联 ADR**：conventions §1.1（Config-First）/ GUARD-08
- **关联偏离点**：无
- **建议处置**：与 TD-USER-02 合 PR。

### TD-USER-04 · `hashIP` 用 SHA-256(ip) 无 pepper，与 `hashPhone` 不对称

- **档位**：3（跨模块加密策略不一致）
- **代码锚点**：`internal/service/user_service.go:hashIP:398`（`crypto/sha256` 直接 Sum256 无 pepper）；对比 `hashPhone:394` 走 `crypto.HashPhone(phone, s.encCfg.PhonePepper)` 用 pepper
- **现状**：IP 哈希做 SMS rate-limit key + PV dedup key（feed_cache.go:hashIPForKey 也是同样问题）。IPv4 空间仅 ~43 亿，纯 SHA-256 可被预计算彩虹表反推；获取到 Redis dump 的攻击者可还原所有 IP。
- **理想**：复用 `cfg.Encryption.PhonePepper` 或加 `cfg.Encryption.IPPepper`，hashIP 改为 `SHA256(ip || pepper)`。
- **触发条件**：合规审计阶段（Step 18 前必查）；Redis 离线泄露事件。
- **重构成本**：M（同时需要在 user_cache key 命名空间里做"加 pepper 后"的 migration —— 旧 ipHash key 会被 mismatch 当成新 IP）
- **关联 ADR**：PRD v3.0 §11.1 / GUARD-04（pepper 决策同源）
- **关联偏离点**：Step 11 偏离点只修了 phone，未一并修 ip
- **建议处置**：Step 18 上线前合并；新增配置后老 key 通过自然 TTL 过期，无需主动迁移。

### TD-USER-05 · SendCode 顺序限流：window 占位后续 daily 失败不回滚，第二次请求体验差

- **档位**：3（跨模块协作 UX）
- **代码锚点**：`internal/service/user_service.go:SendCode:88-123`（三段顺序：window → phone daily → ip daily）
- **现状**：window SETNX 成功 → phone daily 超限 → 直接 return `ErrSMSDailyPhone`，但 sms:window:<phoneHash> 仍占着 60s。用户再点会先撞 window 错误（不准确的 "too frequent" 信息），实际触发限流的是 daily。错误归因被遮蔽。
- **理想**：phone-daily / ip-daily 失败时 `c.rdb.Del(ctx, keySMSWindow(phoneHash))`；保证用户看到最具体的 err code。
- **触发条件**：用户接近 daily 上限时（如本日已发 4/5 次）每次 60s 内重试只看到 window 错误，不知道实际原因。
- **重构成本**：S（半天）
- **关联 ADR**：无（CLAUDE.md §三 用户体验隐含）
- **关联偏离点**：无
- **建议处置**：小 PR 修。

### TD-USER-06 · `UserRepository.UpdateProfile(map[string]interface{})` 无字段白名单，依赖调用方自律

- **档位**：1（防御深度违规）
- **代码锚点**：`internal/repository/user_repository.go:UpdateProfile:101-109`（注释自承认 "caller is responsible for whitelisting"）；当前唯一调用方 `internal/service/user_service.go:UpdateProfile:305` 是安全的（手动选 nickname / bio / avatar_url 三键）。
- **现状**：repository 接口让 service 传任意键值进 UPDATE。若新调用方（如 step 10 audit consumer 或 step 16 metrics consumer）误传 `status / phone_hash / phone_encrypted`，可越权改禁用状态、改手机号映射。
- **理想**：接口签名改为 `UpdateProfile(ctx, id int64, fields UserProfileUpdate)` 其中 `UserProfileUpdate{Nickname, Bio, AvatarURL *string}`；repository 内部转 map。强类型 + 编译期保证。
- **触发条件**：未来加新调用点忘记白名单；review 漏掉。
- **重构成本**：S（半天）
- **关联 ADR**：CLAUDE.md §九（防御深度）
- **关联偏离点**：无
- **建议处置**：本批后 PR；动该接口签名时同步检查所有调用方。

### TD-USER-07 · `GetMyProfile` DecryptPhone 失败静默 fallback 空串，无 metrics 暴露

- **档位**：4（可观测性盲点）
- **代码锚点**：`internal/service/user_service.go:GetMyProfile:265-276`（zap.Warn 后 `plain = ""`）
- **现状**：密钥轮换或 cipher 损坏时用户看到 `phone_masked=""`，但 Prometheus 没有计数器 → 运维无感知。
- **理想**：暴露 `cooking_phone_decrypt_failed_total{user_id_bucket=...}` Counter；Grafana alert 阈值 > 0 per 5min 触发告警。
- **触发条件**：密钥滚动期间未完成迁移 + 老数据未重加密时；不立即出事故，但调试无从下手。
- **重构成本**：S（< 1h，Step 16 已经有 pkg/metrics）
- **关联 ADR**：体检 §5 / PRD v3.0 §11.1
- **关联偏离点**：Step 11 偏离点（fallback 设计）记录了 fallback，未提 metrics
- **建议处置**：Step 16 Metrics 收口 PR 时一起加。

### TD-USER-08 · `migrate-phone` CLI 无 `--resume` / lastID checkpoint，中断后必须重头扫表

- **档位**：4（一次性工具妥协）
- **代码锚点**：`cmd/migrate-phone/main.go:migrate:94-165`（lastID 在内存中递增，无落盘）
- **现状**：百万行用户表迁移时若中断（容器重启 / OOM），下次启动 lastID 从 0 重新扫；computeUpdates 内部能识别"已加密"行（解密成功）跳过，但仍需逐行 SELECT，30+分钟空跑。
- **理想**：把 lastID 持久化（文件 / `migrate_state` 表 / Redis），重启时 read back。
- **触发条件**：prod 50w+ 用户量执行 migrate-phone 时；当前 dev/staging 数据量小不痛。
- **重构成本**：S（< 1 天）
- **关联 ADR**：无（Step 11 一次性脚本，PRD 未单列 ADR）
- **关联偏离点**：无
- **建议处置**：Step 18 上线 prod 数据迁移前补；若 prod 数据量小（< 10w）可永久接受。

### TD-USER-09 · JWT Manager 仅暴露 HS256，Refresh Token 服务端不存任何状态 → 无法主动失效

- **档位**：2（架构所限）
- **代码锚点**：`pkg/jwt/jwt.go:81-83`（IssueRefreshToken 注释自承认"compromise of a refresh token therefore allows full account takeover until expiry"）
- **现状**：refresh token 无 Redis 状态，logout 只 blacklist access token jti；refresh 仍然有效直到 168h 自然过期。攻击者拿到 refresh token 后用户改密码也无法立即踢出。
- **理想**：refresh token 落 Redis `jwt:rt:<jti>`，set on issue / del on logout；refresh 时 SCRIPT 内 GET-then-DEL 实现单次使用（rotation）。
- **触发条件**：用户安全事件（设备丢失 / 密码泄露）需要立刻失效所有 session 时；面试官追问"refresh token 怎么主动失效"时也答不出。
- **重构成本**：M（jwt + user_cache + user_service 联动）
- **关联 ADR**：PRD §11（认证设计）
- **关联偏离点**：无
- **建议处置**：Step 18 上线前评估；MVP 阶段接受，写进"已知限制"。

### TD-POST-01 · `AuthorAssembler.LoadMap` 对独立作者 ID 顺序 N+1 查询，无 `FindByIDs` 批量加载

- **档位**：1（违反 A2 Repository 模板：批量场景必须批量查询）
- **代码锚点**：`internal/service/author_assembler.go:LoadMap:78-89`（`for _, uid := range uniqueAuthorIDs(posts) { LoadOne(ctx, uid) }`）；`internal/repository/user_repository.go:UserRepository:32-37` 接口未定义 FindByIDs；`post_repository.go:36-38` 自承认"if 100+ posts ever pull, add FindByIDs(ids)"
- **现状**：feed 一页 size=20 → 最坏 20 次 SELECT * FROM users WHERE id=?；search 复用同一 assembler 同样问题。每次 DB roundtrip ~1ms，P99 累积 20+ms 单纯花在等 DB；DBResolver 也未 batch。
- **理想**：`UserRepository.FindByIDs(ctx, ids []int64) (map[int64]*User, error)` 单 SQL `WHERE id IN (...)`；LoadMap 一次性拉齐。
- **触发条件**：双实例 + 多 scene 后 feed QPS 上来时；Step 15 已上线，正在咬。
- **重构成本**：M（接口 + 实现 + 单测 + 替换调用点）
- **关联 ADR**：CLAUDE.md §七 Repository 模板 / PRD v3.0 §5.2
- **关联偏离点**：无
- **建议处置**：本批后立即修；与 search 模块（Batch 3）共享收益。

### TD-POST-02 · Feed 游标仅 `created_at`，同毫秒插入会重复或漏数据（自承认 MVP 妥协）

- **档位**：4（MVP 妥协 + 自承认）
- **代码锚点**：`internal/repository/post_repository.go:ListFeed:223-232`（注释 "True dedup would require a (created_at, id) tuple cursor — overkill for MVP volume"）
- **现状**：DATETIME(3) 仍可能同毫秒并发 INSERT（migration 灌种 / 压测）；游标 `created_at < ?` 严格小于会跳过同毫秒尾巴的帖子。
- **理想**：cursor 升级 `(createdAtMs, id)` tuple，签名 + base64 编码；查询 `WHERE (created_at, id) < (?, ?)`。
- **触发条件**：批量 seed / migration / 真实流量 burst（>1000 帖/s）。
- **重构成本**：M（cursor schema 改造 + 客户端兼容）
- **关联 ADR**：PRD v3.0 §5.2 / §6.3
- **关联偏离点**：无
- **建议处置**：Step N 重估（用户量 > 10w 时）；当前 MVP 接受。

### TD-POST-03 · `CreatePostReq.Content` 无 sanitize，控制字符 / 零宽字符 / 潜在 XSS payload 直接入库

- **档位**：1（输入卫生缺失）
- **代码锚点**：`internal/service/post_service.go:Create:98-179`（无 sanitize，仅 strings.TrimSpace title）；`internal/model/dto/post.go:CreatePostReq:60-66`（binding 只有 max=5000）
- **现状**：content 字段是 TEXT，前端是否转义依赖前端框架。SSR 渲染（如未来上 Next.js / Nuxt）或第三方 SDK 抓 content 时存在 XSS；零宽字符 / RTL override / 控制字符可用于钓鱼标题（已通过 title trim 缓解但 content 没缓解）。
- **理想**：service 层调 `bluemonday` 或自写 sanitize（剥控制字符 + HTML escape 关键字符）；同样适用于 step.Text。
- **触发条件**：上 SSR 时（中长期）；安全审计阶段被标红（短期）。
- **重构成本**：S（半天，引入 pkg/sanitize）
- **关联 ADR**：CLAUDE.md §九（防御深度）
- **关联偏离点**：无
- **建议处置**：Step 18 上线前补；与 TD-POST-06 合并。

### TD-POST-04 · Feed cache unmarshal 失败时仅 warn 不主动 DEL key，损坏 key 会反复触发 warn

- **档位**：3（缓存自愈缺失）
- **代码锚点**：`internal/service/post_service.go:ListFeed:249-258`（`zap.Warn("feed cache unmarshal failed; refetching from db", ...)`，无 c.feedCache.Del）
- **现状**：cache 数据损坏（如 partial write / 序列化版本不兼容）时落 DB 重算 + 重新 SetFeed 覆盖。但若 SetFeed 失败 → 下次请求又 hit 同一坏 key 又 warn。日志噪声。
- **理想**：unmarshal 失败时主动 `c.feedCache.Del(ctx, key)` 清掉坏 key；FeedCache 加 `Del(ctx, scene, version, cursor)` 方法。
- **触发条件**：上线后第一次 dto.FeedResp 结构调整（如加字段时旧实例尚未滚动完，写入老格式被新实例读到）。
- **重构成本**：S（< 1h）
- **关联 ADR**：无
- **关联偏离点**：无
- **建议处置**：Step 18 之前合并。

### TD-POST-05 · `Create` 的三步异步副作用顺序不严格：publishPostEvent / publishAuditEvent / bumpFeedVersion 之间无 happens-before

- **档位**：3（消息编排）
- **代码锚点**：`internal/service/post_service.go:Create:169-171`
- **现状**：三个调用串行但都 fire-and-forget。`bumpFeedVersion` 先成功 → `publishAuditEvent` 失败时帖子 invisible 但 feed:ver 已 INCR → 接下来 5min 内所有 scene feed 全部 cache miss 走 DB，纯亏。理论上 audit 失败的帖子永远 invisible，根本不该 bump cache。
- **理想**：先 publish*Events → 都成功后再 bumpFeedVersion；或：bumpFeedVersion 移到 AuditConsumer 成功 flip is_visible=1 时（更精准但跨模块）。
- **触发条件**：RabbitMQ 偶发 publish 失败时；切 confirm 模式后更明显。
- **重构成本**：S（顺序调整）
- **关联 ADR**：PRD v3.0 §3.1 / §6.3（feed:ver 设计）
- **关联偏离点**：无
- **建议处置**：与 Batch 5 audit consumer 收口时一起重新评估。

### TD-POST-06 · `CreatePostReq.Steps[].Text` 仅校验 `required,max=500`，未 trim 即接受全空格 step

- **档位**：1（输入卫生）
- **代码锚点**：`internal/model/dto/post.go:PostStepReq:75`（`binding:"required,max=500"`）；`internal/service/post_service.go:Create:121-125`（service 只校验 ImageURLs 白名单，不 trim text）
- **现状**：客户端可投 `{"text": "   "}` 通过 required 校验；模型存 `text="   "` 进库。
- **理想**：service 层 `strings.TrimSpace(st.Text)`；TrimSpace 后空则 reject 为 `ErrPostStepsInvalid`。
- **触发条件**：恶意客户端 / 调试客户端 / mobile 软键盘自动加空格。
- **重构成本**：S（< 30min）
- **关联 ADR**：CLAUDE.md §九
- **关联偏离点**：无
- **建议处置**：与 TD-POST-03 合 PR。

### TD-POST-07 · Feed 全局版本号 `feed:ver` 粒度过粗，单个 scene 写入会失效所有 scene cache

- **档位**：2（架构所限）
- **代码锚点**：`internal/cache/feed_cache.go:feedVersionKey:88`（全局单 key）；`internal/service/post_service.go:bumpFeedVersion:348` 任何新帖都 INCR
- **现状**：发一条"减脂餐"帖会让"出租屋""快手日常"等所有 scene 的所有 cursor 缓存集体失效。MVP 写少时无感；MAU 上来后写频繁，cache miss 率飙升 → MySQL feed 查询变热点。
- **理想**：按 scene 分版本号 `feed:ver:s0`（全部）+ `feed:ver:s{1..8}`；写新帖时 INCR 两个（"全部" + 自己 scene），其它 scene 缓存不受影响。
- **触发条件**：MAU > 50w / 日活 > 5w 时；当前 MVP 远未到。
- **重构成本**：M（FeedCache 改造 + key 命名规则同步 PRD §6.3）
- **关联 ADR**：PRD v3.0 §6.3 / GUARD-03
- **关联偏离点**：无
- **建议处置**：Step N 重估；触发条件未到不动。

### TD-POST-08 · `PostStep` 无 `UpdatedAt`，未来支持编辑帖子时无法追溯修改时间

- **档位**：2（架构所限：MVP 不支持编辑步骤）
- **代码锚点**：`internal/model/post_step.go:PostStep:88-95`（只有 CreatedAt）；`migrations/000005_create_post_steps_table.up.sql`（同）
- **现状**：MVP 明确不支持 edit post，步骤只能 create-once。未来若开放编辑（PRD 已暗示），无 UpdatedAt → 无法判断哪些 step 被改过、是否需重审。
- **理想**：先加 `updated_at DATETIME(3)` 列，写默认 = created_at；待 edit 功能上线时 ON UPDATE 维护。
- **触发条件**：开放 edit post 功能时。
- **重构成本**：S（一条 migration + GORM 字段）
- **关联 ADR**：PRD-Phase2 §F-C01
- **关联偏离点**：无
- **建议处置**：触发时再做；当前 MVP 接受。

### TD-POST-09 · `parsePathID` 在 handler 包内定义但 `user.go` 用 `strconv.ParseInt` 直接处理，风格不一

- **档位**：4（美学不一致）
- **代码锚点**：`internal/handler/post.go:parsePathID:110-119`（被 `feed.go:78` 复用）；`internal/handler/user.go:GetPublicProfile:118`（直接 strconv.ParseInt + id <= 0 检查重复一遍）
- **现状**：两处做同一件事，一处包成 helper，一处展开。新手会困惑哪个是规范。
- **理想**：所有 handler 走 parsePathID；统一抽到 `internal/handler/helpers.go` 或 `pkg/response/path.go`。
- **触发条件**：新人加新 handler 时倾向复制 user.go 的展开模式 → 散布更多。
- **重构成本**：S（< 30min）
- **关联 ADR**：CLAUDE.md §七 Handler 模板
- **关联偏离点**：无
- **建议处置**：低优先；与下一次 handler 模块变更 piggyback。

---

## 本批批注（收口时填写）

- **档位分布**：档 1 = 7（TD-USER-01 / TD-USER-02 / TD-USER-03 / TD-USER-06 / TD-POST-01 / TD-POST-03 / TD-POST-06）/ 档 2 = 3（TD-USER-09 / TD-POST-07 / TD-POST-08）/ 档 3 = 4（TD-USER-04 / TD-USER-05 / TD-POST-04 / TD-POST-05）/ 档 4 = 4（TD-USER-07 / TD-USER-08 / TD-POST-02 / TD-POST-09）；合计 18 条
- **关键发现**：
  1. **UserCache 设计了完整缓存但全链路未接**（TD-USER-01）—— A1 模板违规，且文档（PRD-Phase2 §3 F-U04）引用了不存在的功能。
  2. **incrAndCheck 未走 Lua**（TD-USER-02）—— 体检 M3 修了 like_cache 漏修 user_cache，B3 规则破窗。
  3. **Author N+1 在 feed / search 共享 assembler 中长期未补 FindByIDs**（TD-POST-01）—— A2 Repository 模板违规，自承认债务但未还。
  4. **输入卫生缺失**（TD-POST-03 + TD-POST-06 + TD-USER-06）—— Content / Step.Text / UpdateProfile map 三处对客户端输入信任过度；防御深度未达 CLAUDE.md §九 要求。
- **新增护栏**：本批未新增。考虑追加一条候选 **GUARD-16 · Feed 游标"故意不用 (created_at, id) tuple"**（与 TD-POST-02 联动 —— 是已知妥协而非疏忽，Final 阶段定夺是否提升为正式护栏）。
- **下一批衔接点**：
  - Batch 3 审 like_consumer / pv_consumer 时复核 like_cache 的 Lua 设计是否覆盖到所有读后写场景（与 TD-USER-02 对照）。
  - Batch 3 审 search_service 时复用 author N+1 结论（TD-POST-01）—— 如有 FindByIDs 实现，搜索路径同样受益。
  - Batch 5 审 audit consumer 时复核 publish + bump 顺序（TD-POST-05）。
  - Batch 6 审 metrics 接入时复核 DecryptPhone 失败 metric（TD-USER-07）。
