# Batch 5 · 审核（Step 10） + 手机号加密（Step 11） + 日志脱敏与错误码收口（Step 12）

> **审计范围**：
> - **审核**：`internal/model/audit_log.go`、`internal/repository/audit_repository.go`、`internal/consumer/audit_consumer.go`、`pkg/audit/`（auditor.go / aliyun.go / mock.go / doc.go）、`migrations/000006_*`
> - **手机号加密**：`pkg/crypto/`（phone.go / mask.go / doc.go / 测试）、`cmd/migrate-phone/main.go` 的加密分支细节
> - **日志脱敏与收口**：`internal/middleware/logger.go`（含 sanitizeQuery）、`internal/middleware/security.go`、`internal/middleware/recovery.go`、`internal/middleware/cors.go`、`internal/middleware/request_id.go`、`pkg/logger/`（logger.go / fields.go）、`pkg/validator/`、`pkg/response/response.go`、`pkg/errcode/errcode.go`（最终段位收口验证）
>
> **审计意图**：合规面（AES-GCM 密钥处理、phone_hash 一致性、日志脱敏覆盖度）+ 错误码段位最终一致性（B1 评分卡）。
>
> **关联材料**：PRD v3.0 §7 / §11.1 / §11.4、progress/10 / 11 / 12 偏离点

---

## 本批条目

<!--
条目模板（复制使用）：

### TD-AUDIT-NN / TD-CRYPTO-NN · <一句话标题>

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

### TD-AUDIT-01 · `AuditConfig` 缺超时配置，Aliyun Green RPC 无 ctx 截止时间

- **档位**：1
- **代码锚点**：`pkg/config/config.go:134-140`（`AuditConfig` 结构体仅含 Provider / AK / Region / MockResult，无 Timeout）+ `pkg/audit/aliyun.go:49-55`（`green.NewClientWithAccessKey` 创建 client 时未注入超时）+ `pkg/audit/aliyun.go:59`（`Review` 接收 `ctx` 但 alibaba SDK 不消费 ctx）
- **现状**：AuditConsumer 是单 goroutine 串行处理：`Review` 调用阻塞约 200-800 ms（正常）或可挂数十秒（Aliyun 默认 SDK 超时）。`AuditConfig` 没有任何 timeout / retry 字段。生产若 Aliyun 端慢响应 / 网络抖动，整条审核管道被首个慢请求阻塞，后续 PostEvent 在 Channel/RabbitMQ 队列里堆积。
- **理想**：`AuditConfig` 新增 `Timeout time.Duration`（默认 5s）和 `MaxRetries int`（默认 2，指数退避）。`AliyunAuditor` 在 `NewAliyunAuditor` 内调 `client.GetConfig().WithTimeout(timeout)`（或封装 `http.Client`）。`Review` 内对临时性错误（503 / context deadline）走 backoff。
- **触发条件**：生产环境永远成立；真正咬人的窗口：① Aliyun Green 区域性抖动 ② 网络出口流量大 ③ AuditConsumer goroutine 数=1 时尤其敏感。
- **重构成本**：S（新增 2 个 cfg 字段 + 3 yaml 默认值；SDK 超时注入 5 行）。
- **关联 ADR**：CLAUDE.md §七 B4 Config-First / conventions §1.1（"超时必须进 config"）
- **关联偏离点**：Batch 4 / Batch 3 / Batch 2 反复出现的 Config-First 半破延续（与 TD-FOLLOW-01 / TD-UPLOAD-02 / TD-SEARCH-01 同根）。
- **建议处置**：与 Config-First 全局清账 PR 一并落地（Batch 4 收口已建议）。本条优先级高于其他 Config-First 半破——审核管道阻塞是 P1 事故。

---

### TD-AUDIT-02 · 单次 API 失败后 post 永久 `audit_status=0` / `is_visible=0`，无重试 / DLQ / reconcile

- **档位**：4
- **代码锚点**：`internal/consumer/audit_consumer.go:155-162`（fail-closed 路径仅 log，不入 DLQ 不重试）+ `internal/consumer/audit_consumer.go:30-34`（注释自陈"a future reconcile job or manual admin action can requeue it"）
- **现状**：`auditor.Review` 返回 error → log 470101 + return。AuditEvent 在 ChannelBus 模式下已被 channel 消费、不再可重投；RabbitMQBus 模式下未配 DLX。被 fail-closed 的帖子卡在 `audit_status=0`（"待审"）状态，与"新提交未处理"无法区分。注释承诺的 reconcile 任务在代码里不存在。
- **理想**：① Step 13 RabbitMQ 切换后给 TopicAudit 配 DLX + 死信重投策略；② 新增 audit-reconcile-cron（每 5 分钟扫 `audit_status=0 AND created_at < now - 5min` 的 post，重新 Publish AuditEvent）；③ 加 audit_status=6（"机审失败待人工"）显式状态区分，避免与"新提交"混淆。
- **触发条件**：Aliyun Green API 整段抖动 / 配额超限 / 网络分区——MVP 阶段确实罕见，但任何一次都会造成"用户发了帖永远看不到"的 P1 客诉。
- **重构成本**：M（reconcile cron + DLX + 新增 audit_status enum，至少 2-3 天）。
- **关联 ADR**：PRD v3.0 §9.2 + audit_consumer.go 文件头 Degradation 章节
- **关联偏离点**：progress/10 偏离点登记的"audit_status=0 复用为提交信号"——刻意决策但**仅覆盖正常路径**，失败路径的状态歧义未被讨论。
- **建议处置**：Step 13 RabbitMQ 切换收口时一起落地（DLX + reconcile）；audit_status 状态拆分推到 Step 18 上线前安全复审一并改。

---

### TD-AUDIT-03 · `audit_logs.Create` 失败后仍走 `UpdateAuditStatus`，合规审计链断裂

- **档位**：1
- **代码锚点**：`internal/consumer/audit_consumer.go:180-195`（先写 audit_logs，写失败仅 log "Do NOT return — still try to update posts" 然后继续 UpdateAuditStatus）
- **现状**：当前流程：① `auditRepo.Create(auditLog)` 失败 → log 470102 + 不 return → ② `postRepo.UpdateAuditStatus` 把 posts.audit_status 置为终态 + `is_visible=1`（若 pass）。结果：帖子可见但**没有对应的 audit_logs 行**，合规审计的"每个状态变更必有一条 trail"假设被打破。注释自陈这是刻意决策（"the log row is append-only, no rollback needed"），但合规视角下"posts 终态 ↔ audit_logs 必存在"是**强不变式**，不可放任违反。
- **理想**：颠倒顺序，audit_logs 优先：先 Create audit_log（失败则 return + 留 post 在 pending 状态等 reconcile），成功后再 UpdateAuditStatus。或：两步包在事务内（GORM Tx），任一失败回滚。或：UpdateAuditStatus 失败时补一条 `audit_status=终态` 的 audit_log（虽然 posts 没改但合规链完整）。
- **触发条件**：MySQL 短暂抖动 / audit_logs 表锁 / 该行违反约束（实际现表无约束，但未来可能加）。监管检查时被发现：post 是 visible 但找不到对应审核证据。
- **重构成本**：S（顺序倒一下 + 当前 try-update-anyway 删掉；单 commit 即可）。
- **关联 ADR**：PRD v3.0 §10 内容安全合规 / migrations/000006 文件头"每次状态流转写一行"
- **关联偏离点**：无登记——此处刻意决策（注释里"Do NOT return"）但**与合规原则矛盾**且没有偏离登记。属未识别的设计失误。
- **建议处置**：立即修。本条是档 1 里最该立即修的——一句话改顺序即可消除合规风险。

---

### TD-AUDIT-04 · `AliyunAuditor.Review` 接收 `ctx` 但 alibaba SDK 不消费 ctx，shutdown 时挂起调用无法取消

- **档位**：2
- **代码锚点**：`pkg/audit/aliyun.go:59-87`（`Review` 签名带 ctx，但 `a.client.TextScan(req)` / `a.client.ImageSyncScan(req)` 都是同步阻塞，未传 ctx）+ `pkg/audit/mock.go:39`（mock 同样忽略 ctx：`Review(_ context.Context, req ...)`）
- **现状**：AuditConsumer 设计是"finish what's in flight, don't start new"（audit_consumer.go 文件头注释）——但实际上 alibaba-cloud-sdk-go v1.x 的 TextScan/ImageSyncScan 不接受 context.Context 参数。父 ctx Cancel 后，正在阻塞的 SDK 调用会持续直到 SDK 内部超时（默认数十秒）。AuditConsumer.Start 因此可能在 shutdown 后挂数十秒，拖累整体优雅停机超时。
- **理想**：要么 ① 用 `pkg/audit/aliyun.go` 自封装 net/http.Client + 手写签名走 Green REST API（完全控制 ctx 透传 + 超时）；要么 ② 在 Review 内启动 goroutine 调 SDK，主 goroutine `select { case <-ctx.Done(): return ctx.Err() case <-done: ... }`（早返回但 SDK goroutine 仍泄漏）；要么 ③ 接受现状但把 shutdown timeout 拉到大于 SDK 超时。
- **触发条件**：rolling restart 期间恰有 audit_consumer 阻塞在 SDK 调用 → 优雅停机超时 → kubelet SIGKILL → in-flight audit event 丢失（ChannelBus 模式）或重投（RabbitMQ at-least-once 模式，但 audit_logs 是 INSERT 无幂等保障，会重复写日志）。
- **重构成本**：M（方案 ① ≥ 2 天，方案 ② 半天但有 goroutine 泄漏，方案 ③ 半小时但增加上线时间）。
- **关联 ADR**：CLAUDE.md §七 A4 Service ctx 透传 / PRD v3.0 §4.3 优雅停机
- **关联偏离点**：进度文档/10 未明确登记"SDK 不消费 ctx"。属未识别偏离。
- **建议处置**：档 2 永久接受 + 短期方案 ③（shutdown timeout 拉到 30s）；长期方案 ① 等下次 Aliyun SDK 升级 / 切自封装 HTTP 客户端时一并改。新增护栏候选 GUARD-17：「优雅停机超时 ≥ 外部 SDK 内部超时」。

---

### TD-CRYPTO-01 · `EncryptPhone(plain, keyHex="")` 静默回落为明文落库，prod 配置漏 key 即破合规

- **档位**：1
- **代码锚点**：`pkg/crypto/phone.go:23-27`（`if keyHex == "" { return plaintext, nil }`）+ `internal/service/user_service.go:175-186`（首次登录自动注册路径直接信任 EncryptPhone 返回值，把它写入 `user.PhoneEncrypted`）
- **现状**：EncryptPhone 设计成"key 为空 → 返回明文"，文件头注释自陈"This allows dev environments to run without configuring a key while keeping the code path identical to production"。但**生产环境忘配 `APP_ENCRYPTION_PHONE_KEY` 完全没有任何报错**——服务正常启动、用户正常注册、users.phone_encrypted 表里全是明文手机号。合规事故的最经典反面教材。`config.validate` 没有"生产模式必须配 PhoneKey"的硬断言。
- **理想**：① 在 `pkg/config/config.go` 的 validate 段加 `if cfg.Environment == "prod" && cfg.Encryption.PhoneKey == "" { return fmt.Errorf("encryption.phone_key required in production") }`；② 或把 EncryptPhone 的空 key 行为改成 panic / 返回 error，强迫 caller 显式选择"我接受明文落库"（dev 路径在调用处兜底）。方案 ① 更友好，但要求项目有显式 `Environment` 字段。
- **触发条件**：运维变更 yaml 时漏配 / env var 没生效 / k8s secret 没挂上 → 全量新用户的手机号以明文形式入库 → 合规审计时一查就破。
- **重构成本**：S（config.validate 加一条 if + 测试一条）。
- **关联 ADR**：PRD v3.0 §11.1 手机号加密 / GUARD-04（双字段设计）
- **关联偏离点**：progress/11 偏离点登记了"dev 模式 key 为空允许跳过加密"——刻意决策，但**未给 prod 配 fail-loud 兜底**。属偏离的一半未到位。
- **建议处置**：立即修。最低成本（5 行 config.validate）防最严重合规事故，没有任何接受空间。新增护栏候选 GUARD-18：「prod 模式 PhoneKey / PhonePepper 必须非空，config.validate 强制」。

---

### TD-CRYPTO-02 · 无 AES key / pepper 轮换路径，rotation 必须停服或破坏已有数据

- **档位**：2
- **代码锚点**：`pkg/crypto/phone.go:23-95`（EncryptPhone / DecryptPhone 都只接受单个 keyHex）+ `pkg/crypto/phone.go:104-107`（`HashPhone(phone, pepper)` 单 pepper）+ `cmd/migrate-phone/main.go:179-216`（迁移工具只接受单 key + 单 pepper）
- **现状**：crypto 包的设计假设"key 和 pepper 是单个静态值"。生产若需轮换密钥（合规要求 / 怀疑泄漏），路径只有：① 停服 → 用旧 key decrypt + 新 key encrypt 全表重写 → 启服。期间 ② 在线轮换不可行，因为 ① DecryptPhone 不接受"老 key 列表"，② HashPhone 改 pepper 直接让所有 phone_hash 失效（uk_phone_hash 唯一索引匹配错位 → 用户登录失败）。这是项目目前合规风险最大的一处"无 rotation 设计"。
- **理想**：① `DecryptPhone` 改为接受 `[]string` 老 key 列表，按顺序尝试，命中即重新 encrypt 用新 key 回写（lazy migration）；② users 表新增 `phone_hash_old` 列在轮换期共存；③ migrate-phone 工具支持 `--old-key` / `--old-pepper` 参数，把全表分批 lazily 迁移；④ key version 元数据写进 ciphertext 头（1 字节 version prefix）。
- **触发条件**：合规审计要求季度轮换 / AccessKey 怀疑泄漏 / 监管事件触发紧急轮换——MVP 阶段不会触发，但任何 SOC2 / ISO 27001 类合规认证都会发现这是 P1 缺口。
- **重构成本**：L（≥ 5 天：ciphertext 格式带 version + DecryptPhone 多 key + users 表迁移 + lazy migration 监控）。
- **关联 ADR**：PRD v3.0 §11.1 + GUARD-04
- **关联偏离点**：progress/11 偏离点提到"pepper 用环境变量注入"——刻意决策，但**未规划轮换路径**。属未识别偏离。
- **建议处置**：档 2 永久接受到合规需求出现；上线前安全复审若被合规方挑战，立刻启动 Step 18 之后的"crypto v2 with rotation"专项。

---

### TD-CRYPTO-03 · `GetMyProfile` decrypt 失败降级为空 `phone_masked`，与 fail-closed 审核原则不一致

- **档位**：3
- **代码锚点**：`internal/service/user_service.go:265-280`（decrypt 失败 → `plain = ""` → `phone_masked = ""`，仅 warn 不 5xx）
- **现状**：用户读取自己资料时若 DecryptPhone 失败（key 轮换 / ciphertext 损坏），service 返回 `phone_masked = ""`（空串）+ HTTP 200。注释承认"can only happen if the key was rotated without re-running the migration script"——但**前端看不出区别**，无法触发"请联系客服恢复手机号"流程。审核（audit_consumer.go）走 fail-closed（API 失败保留 invisible）；手机号读取走 fail-open（解密失败返回空）。两套相反原则未在一处统一登记。
- **理想**：decrypt 失败时返回 errcode.ErrDecryptPhone（480102）或 errcode.ErrServer，让客户端弹"账号数据异常"提示。同时埋点告警 `phone_decrypt_failed_total`——这是 P0 信号，绝不应该静默。
- **触发条件**：① 运维误操作改了 PhoneKey 没跑迁移；② ciphertext 列被外部数据修复脚本损坏。MVP 阶段罕见，但发生即"用户登录正常但看不到自己手机号"，客服支持负担飙升。
- **重构成本**：S（return errcode 而非空串 + 加 Prometheus 计数器）。
- **关联 ADR**：CLAUDE.md §九 + PRD v3.0 §11.1
- **关联偏离点**：与 GUARD-15（JWT 黑名单 fail-open）做对比——JWT 走 fail-open 因为黑名单只是"附加防御"；phone 走 fail-open 没有正当理由，本身就是 PII 主路径。属与 GUARD-15 形似但本质不同的偏离。
- **建议处置**：与 Step 16 Prometheus 接入配套——加一个 `phone_decrypt_failed_total` 计数器 + Grafana 告警，同时改成 fail-closed 返回 500。

---

### TD-LOG-01 · `sanitizeQuery` 仅覆盖 URL query 段，POST body / `c.Errors` 不脱敏

- **档位**：3
- **代码锚点**：`internal/middleware/logger.go:13-39`（`sensitiveQueryParams` + `sanitizeQuery` 只处理 RawQuery）+ `internal/middleware/logger.go:68-71`（`c.Errors` 直接 `zap.Error(e.Err)` 拼进 fields，无脱敏链）
- **现状**：① Gin 默认不 log body，POST `/api/v1/users/send-code` 携带 phone 不会被记录——OK；但 ② handler 内 `c.Error(err)` 链上的 error 若 wrap 了 phone（如 `fmt.Errorf("create user phone=%s: %w", phone, err)`）会被原样 zap.Error 出去；③ 项目其他模块在 zap.L().Error() 时若不显式用 `logger.MaskedPhone` 而是 `zap.String("phone", phone)`，全局没有 hook 拦截。脱敏依赖每个 caller 自觉调 `pkg/logger/fields.go` 的 helper。
- **理想**：① 在 zap core 上加一个 redact-on-write hook，按 key 名（"phone" / "code" / "token"）自动 mask；② 或新建 `pkg/logger.SafeLogger` 包装层强制 caller 走 MaskedXxx；③ 全仓 grep `zap\.String\("phone"` 把所有 caller 改成 MaskedPhone（一次性 PR）。
- **触发条件**：永远成立——每加一个新模块、每加一个 error path，就多一处可能漏脱敏的口子。生产 grep 日志找 phone 模式时被发现。
- **重构成本**：M（方案 ① zap hook 半天 + 全仓回归测试；方案 ③ 全仓 grep + 替换 ≤ 1 天）。
- **关联 ADR**：PRD v3.0 §11.4 日志脱敏 / CLAUDE.md §十三"日志格式一致性"
- **关联偏离点**：progress/12 偏离点登记"日志脱敏在 helper 层落实，未做 zap hook"——刻意决策但承认有覆盖盲区。
- **建议处置**：档 3 触发时修；与 Step 16 Prometheus 接入配套——加一个 `phone_in_log_total` 静态扫描（CI grep `zap\.String\("phone"` 阻断），比加 runtime hook 成本低。

---

### TD-CORS-01 · `Access-Control-Allow-Origin: *` 硬编码，注释承诺"prod 收紧"但无 cfg 字段

- **档位**：1
- **代码锚点**：`internal/middleware/cors.go:10-23`（`c.Header("Access-Control-Allow-Origin", "*")` 硬编码 + 注释"Tighten AllowOrigins in production via config before go-live"）
- **现状**：CORS 中间件全模式都返回 `Allow-Origin: *`，没有任何配置开关。注释自陈"prod 走 config 收紧"——但 `pkg/config/config.go` 里**根本没有 CORS 段**。上线前如不手动 patch 这个文件，prod 仍然是 `*`。配合 `Allow-Credentials` 未设导致带 cookie 攻击受限，但前端若改 Bearer Token 模式 + `Allow-Origin: *` 依旧暴露 cross-site CSRF 攻击面。
- **理想**：① `pkg/config/config.go` 新增 `CORSConfig { AllowOrigins []string }`；② 中间件读 cfg 决定 origin（精确匹配请求头里的 Origin，命中才返回该值；不命中拒绝 CORS）；③ dev yaml 默认 `["*"]`、prod yaml 默认 `["https://cooking.example.com"]`。
- **触发条件**：上线前若漏改 / 紧急上线时被忽略 → 任意第三方网站可发 fetch 调本站 API（CSRF + 信息泄露面扩大）。
- **重构成本**：S（cfg 新增 + cors.go 改 20 行 + 3 yaml 同步）。
- **关联 ADR**：CLAUDE.md §七 B4 Config-First / PRD v3.0 §11（安全）
- **关联偏离点**：与 TD-AUDIT-01 / Batch 4 TD-UPLOAD-02 / 等等 Config-First 半破同源——但本条直接关乎线上安全，优先级最高。
- **建议处置**：立即修。Config-First 全局清账 PR 里本条独立作为 P0；不允许 v1.0 上线前还带这段硬编码 CORS。

---

### TD-ERRCODE-01 · `errcode.go` 文件头"X=4 客户端 / X=5 服务端"约定与 470/480 模块自相矛盾

- **档位**：3
- **代码锚点**：`pkg/errcode/errcode.go:3-13`（注释"X : HTTP category prefix (4 = client-facing, 5 = server-internal)"+ "Modules 70 and 80 are server-internal infrastructure errors (HTTP 500). Their X prefix is 4 by convention"）+ `pkg/errcode/errcode.go:111-127`（470101 / 480101 HTTP 500 但 X=4）
- **现状**：文件头同时给出两条规则：① "X=4 客户端 / X=5 服务端"；② "Audit / Encryption HTTP 500 但 X=4"。规则 ② 推翻规则 ①，但规则 ② 又只是"by convention"——无 ADR、无偏离点登记。新成员读规则 ① 选择新错误码段位时容易选错（比如内容审核重试错误想用 570103 还是 470103？）。文档自相矛盾本身就是档 3 跨模块债——单看每条规则都对，合在一起规范半破。
- **理想**：选择并固化一条：① 把 audit / encryption 改成 570 / 580（成本：errcode.go 改两段 + 仓内 grep 替换 470101 / 470102 / 480101 / 480102 大约 ≤ 10 处）；② 或把文件头"X=4 客户端 / X=5 服务端"规则废除，改成"X 与 HTTPStatus 解耦，每个模块自选段位"。方案 ① 更符合 HTTP 直觉，方案 ② 更符合现状（成本 0）。
- **触发条件**：永远成立——新模块加错误码时反复需要在两条规则之间选择，每次选择都浪费 review 时间；不一致也降低 PRD 可信度。
- **重构成本**：方案 ① = S（≤ 半天）/ 方案 ② = XS（改文档即可）。
- **关联 ADR**：CLAUDE.md §七 B1 错误码段位 / PRD v3.0 §7
- **关联偏离点**：progress/12 偏离点登记"audit / encryption 用 X=4 前缀"——刻意决策但**与 §7 自定义规则矛盾**未在偏离表标记。
- **建议处置**：与 Step 17 PRD v3.0 生成 / Final 收口时一并选定方案。本台账记录后由 Final 三视图索引推动一次性收尾。

---

### TD-ERRCODE-02 · `requestIDKey` 常量在 `pkg/response` 和 `internal/middleware` 各定义一份，drift 风险

- **档位**：1
- **代码锚点**：`pkg/response/response.go:13`（`const requestIDKey = "X-Request-ID"`）+ `internal/middleware/request_id.go:9`（`const requestIDKey = "X-Request-ID"`）
- **现状**：两处常量都是 `"X-Request-ID"`，硬编码字符串一致。任一处修改（比如改成 `"X-Trace-ID"`）而另一处忘改，会导致响应 JSON 的 `request_id` 字段为空——客户端日志关联失效，难复现的客服 issue 来源。Recovery middleware（`internal/middleware/recovery.go:21,27`）也单独取 `c.GetString(requestIDKey)`——第三处。
- **理想**：抽到 `pkg/constants/http.go`（或 `pkg/middleware.go` 顶层包级 const）的单一 `RequestIDKey`，三处全部 import。或更彻底：把 `getRequestID(c *gin.Context) string` helper 也抽出去，避免每个文件复制。
- **触发条件**：永远成立——下一次有人重命名 header（比如对齐云厂商 trace ID 头 `X-Cloud-Trace`），漏改概率为 1/3。
- **重构成本**：S（≤ 半天：新建 const + 3 处 import 替换 + grep 验证）。
- **关联 ADR**：CLAUDE.md §十三 + 工程一致性
- **关联偏离点**：无登记——属基础设施代码味，未识别。
- **建议处置**：立即修。最低成本消除 drift 风险。可与 TD-FOLLOW-03（parseUserIDParam 抽公共 helper）合并为 "internal/handler/helpers + pkg/constants 抽取"一次性 PR。

---

## 本批批注

- **档位分布**：档 1 = 5（TD-AUDIT-01 / TD-AUDIT-03 / TD-CRYPTO-01 / TD-CORS-01 / TD-ERRCODE-02） / 档 2 = 2（TD-AUDIT-04 / TD-CRYPTO-02） / 档 3 = 3（TD-CRYPTO-03 / TD-LOG-01 / TD-ERRCODE-01） / 档 4 = 1（TD-AUDIT-02） = 10
- **关键发现**：
  1. **合规面三处档 1**：TD-AUDIT-03（audit_logs 失败仍 UpdateAuditStatus → 合规链断）+ TD-CRYPTO-01（EncryptPhone 空 key 静默退化 → 明文落库）+ TD-CORS-01（Allow-Origin `*` 硬编码无 cfg）——三处都是"低成本（≤ S）但触发即 P0 / P1"的典型，应在 v1.0 上线前合并为「合规清账 PR」一次性消除。
  2. **审核管道四面缺口**：① 无 timeout / retry（TD-AUDIT-01） ② 无 reconcile / DLQ（TD-AUDIT-02） ③ audit_logs 写失败容忍（TD-AUDIT-03） ④ SDK 不消费 ctx（TD-AUDIT-04）。这是项目目前**单模块债务密度最高**的一块。Step 13 RabbitMQ 切换若不一并处理 ②（DLX + reconcile），生产将持续暴露"用户发了帖永远看不到"的尾部故障。
  3. **crypto rotation 缺口 + fail-open 矛盾**：TD-CRYPTO-02（无 key/pepper 轮换路径）是 SOC2 / ISO 27001 类合规审计必踩坑；TD-CRYPTO-03（decrypt 失败 fail-open）与 audit fail-closed 形成原则不一致——同一项目两套相反 fallback 哲学，会让安全复审报告写得很长。
  4. **errcode 文档自相矛盾**（TD-ERRCODE-01）：X=4/5 规则与 470/480 段位互斥，Step 17 PRD v3.0 生成时必须选边并落定，不允许两条规则并存进 v3.0 文档。
- **新增护栏候选**：
  - **GUARD-17 候选**：「优雅停机超时 ≥ 所有外部 SDK 内部超时」（配合 TD-AUDIT-04）。
  - **GUARD-18 候选**：「prod 模式 `Encryption.PhoneKey` / `Encryption.PhonePepper` 必须非空，由 `config.validate` 强制」（配合 TD-CRYPTO-01）。
  - Batch 4 候选的 GUARD-16（OSS URLPrefix 尾 `/`）仍待 Final 阶段定稿。
- **下一批衔接点**：
  - **Batch 6（生产基础设施 Step 13-17）**：审 ① RabbitMQ 切换是否给 TopicAudit 配 DLX、是否有 reconcile cron；② MySQL 主从切换时是否处理 audit_logs 写主库前 panic 的双写不一致；③ Prometheus 是否暴露 `audit_pipeline_*` / `phone_decrypt_*` 指标；④ CI 是否阻断 `zap\.String\("phone"` 这种漏脱敏 PR。
  - **本批遗留**：TD-CRYPTO-02 是 L 级重构，建议 Final 跨模块章节单独起一节 `TD-SYS-CRYPTO-ROTATION` 与未来 SOC2 / 合规专项对齐；TD-AUDIT-04 与 GUARD-17 候选同样推到 Final 落定。
