# Batch 1 · 公共抽象与基础设施基座

> **审计范围**：
> - `internal/event/`：types.go / bus.go / channel.go / rabbitmq.go / doc.go
> - `pkg/errcode/errcode.go`
> - `pkg/config/config.go`
> - `pkg/sms/`：sender.go / mock.go / aliyun.go / doc.go
> - 6 个模板锚点文件二次复审（cache/consumer/repository/service/handler/sms 三件套）
>
> **审计意图**：审"基座是否真的稳"。基座层的债务会向上游每个业务模块传染，必须最严判。
>
> **关联材料**：PRD v3.0 §3 / §7 / §8、conventions §1.1 / §1.3 / §1.4、体检 §2 / §3 / §5.4

---

## 本批条目

### TD-EVT-01 · Event 信封 ID/Timestamp 与 Payload 内字段重复

- **档位**：3
- **代码锚点**：`internal/event/types.go:23-39`（`Event.ID`/`Event.Timestamp` 与 `LikeEvent.EventID`/`LikeEvent.Timestamp` 重复，UnlikeEvent / PVEvent / AuditEvent / CountEvent / PostEvent / FollowEvent / UnfollowEvent 同样模式）
- **现状**：Publisher 在外层 `Event{ID, Timestamp}` 与内层 payload `EventID/Timestamp` 各填一次；Consumer 端只读外层 `Event.ID`（见 `RabbitMQBus.handleDelivery:387`），payload 内字段实际未被消费。
- **理想**：payload 结构体删除 `EventID`/`Timestamp`，统一通过外层 `Event` 字段访问。
- **触发条件**：未来再加新事件类型时容易"按葫芦画瓢"复制冗余字段；payload 序列化体积持续偏大（每条事件多 ~60B）。
- **重构成本**：M（需同步改 8 个 payload 结构 + service 层 publish 调用点 + Consumer 中对应反序列化点，并跑端到端冒烟）
- **关联 ADR**：PRD v3.0 §3.1（EventBus 接口抽象）
- **关联偏离点**：无
- **建议处置**：留待 Batch 3 / Batch 4 对各 Consumer 审计时同步评估。新 Topic 务必只用外层 Event 字段；已有 Topic 待"统一重构事件结构"专项 PR。

### TD-EVT-02 · ChannelBus 队列满即丢弃，无 Metrics 暴露

- **档位**：4
- **代码锚点**：`internal/event/channel.go:Publish:53-65`（`select { case ch <- payload: default: zap.Warn }`）
- **现状**：订阅者 channel 满时静默丢弃，只打 Warn 日志；Prometheus 无 drop 计数器；dev/CI 跑过时无人会注意到。
- **理想**：暴露 `cooking_event_bus_dropped_total{topic=...}` Counter；dev 模式同时输出告警阈值（如 30s 内 >100 条触发停启动失败）。
- **触发条件**：dev/CI 高并发压测时静默丢事件 → like_count 调试不可信；生产切 RabbitMQ 后此实现仍被单元测试使用，掩盖业务侧死锁。
- **重构成本**：S（pkg/metrics + ChannelBus 注入 Counter，约半天）
- **关联 ADR**：PRD v3.0 §3.2（ChannelBus 仅 dev / 测试用途）
- **关联偏离点**：无
- **建议处置**：留 Step 18+ 引入"测试稳定性"专项时统一加。生产期保留为"已知 dev 限制"，写进 conventions。

### TD-EVT-03 · ChannelBus bufSize 来自 NewChannelBus 入参，不在 Config

- **档位**：4
- **代码锚点**：`internal/event/channel.go:NewChannelBus:29`（`bufSize int`）；`cmd/server/main.go` 中目前用硬编码常量装配
- **现状**：bufSize（每个订阅者 channel 容量）写在 main.go 的字面量里，无法 yaml 调整；与 §七 Config-First 规则冲突。
- **理想**：进 `pkg/config/config.go` 的 `MQConfig.ChannelBufSize`，default=1024。
- **触发条件**：dev 想压测吞吐量时需要改代码 → 违反 Config-First；新人 onboarding 时需阅读 main.go 才知道该值。
- **重构成本**：S（< 1h）
- **关联 ADR**：conventions §1.1（Config-First）
- **关联偏离点**：无
- **建议处置**：可与 TD-CFG-02 合并一次 PR 修复。

### TD-EVT-04 · Handler 错误处理不统一：Channel 仅 Warn，RabbitMQ 直入 DLX

- **档位**：1
- **代码锚点**：`internal/event/channel.go:Subscribe:103-107`（handler err 只打 Warn 继续消费）vs. `internal/event/rabbitmq.go:handleDelivery:395-399`（handler err → Nack false → DLX，无 retry）
- **现状**：同一 handler 在 dev（Channel）与 prod（RabbitMQ）的失败语义完全不同。dev 上 handler 永远"消费成功"，prod 上首次失败立即 DLX。
- **理想**：抽象出"transient error"与"poison error"两类；transient 在两端都按相同策略重试（如 Channel 端入临时 buffer 重试 3 次，RabbitMQ 端用 retry-queue + ttl）。当前只有 PV/Like Consumer 内部自带 retry，对 Audit/Count 不友好。
- **触发条件**：切 RabbitMQ at-least-once 后（已切，PRD v3.0 §3.2），任意 handler 偶发 MySQL deadlock 即被 DLX，事件永久丢失；dev 单元测试无法暴露此问题。
- **重构成本**：M（设计 retry 协议 + 在两个实现里对齐 + Consumer 端配合改造）
- **关联 ADR**：PRD v3.0 §3.2 / 体检 §3
- **关联偏离点**：无
- **建议处置**：Batch 3 / Batch 5 审 Consumer 时同步评估具体 handler 是否需要 retry-queue；先记录，不立即动。

### TD-MQ-01 · RabbitMQ 拓扑常量硬编码在文件内，未走 Config

- **档位**：2
- **代码锚点**：`internal/event/rabbitmq.go:73-77`（`rabbitMQExchange = "cooking.events"` / `rabbitMQDLXExchange` / `rabbitMQDLXQueue`）+ `queueName:196`（`"cooking." + topic`）
- **现状**：交换器名 / DLX 名 / 队列前缀全部为包级 const，无法多环境隔离（dev/staging/prod 同 broker 时会撞）。
- **理想**：进 `MQConfig.ExchangePrefix`（默认 `cooking`），运行时拼装 `<prefix>.events`、`<prefix>.<topic>`。
- **触发条件**：未来引入共享 broker（如多个项目复用同一个 RabbitMQ 集群）→ 命名冲突；蓝绿部署时新旧版本队列名相同 → 双消费者抢同一队列。
- **重构成本**：S（拓扑名生成函数化即可）
- **关联 ADR**：PRD v3.0 §3.3（RabbitMQ 拓扑）
- **关联偏离点**：无
- **建议处置**：Step 18 部署阶段如确有共享 broker 计划再动；当前单 broker 场景接受。

### TD-MQ-02 · Publish 全局 mutex 串行，无分片或 channel pool

- **档位**：3
- **代码锚点**：`internal/event/rabbitmq.go:Publish:251-253`（`b.mu.Lock(); b.pubCh.PublishWithContext(...); b.mu.Unlock()`）
- **现状**：所有 Publish 调用串行通过同一个 `b.pubCh`。amqp091-go 的 Channel 自身串行化写，但全局 mutex 使等待时间叠加；高 QPS 下成为单点。
- **理想**：维护一个 publish channel pool（按 P95 QPS 决定大小，默认 4-8），轮询写入；或用 amqp 的 Confirm 模式 + 异步 publish。
- **触发条件**：双实例部署后单实例 publish QPS > 2k/s（PV 事件 + Like 事件 + Count 事件）时 publish latency P99 > 50ms。
- **重构成本**：M（pool 实现 + reconnect 协调）
- **关联 ADR**：体检 §3 / PRD v3.0 §3.2
- **关联偏离点**：无
- **建议处置**：上线后接 Prometheus `publish_duration` 直方图观察；P99 < 50ms 时不动。

### TD-MQ-03 · Subscribe 未设 BasicQos / Prefetch，多实例公平消费失衡

- **档位**：1
- **代码锚点**：`internal/event/rabbitmq.go:subscribeOnce:323-357`（`subCh.Consume` 之前未调用 `subCh.Qos`）
- **现状**：默认 prefetch=unlimited。多实例下任一实例先抢到队列，broker 会把剩余消息全部 push 给它的 channel buffer，另一实例长期空闲。
- **理想**：在 Consume 之前 `subCh.Qos(prefetchCount, 0, false)`，prefetchCount 从 `MQConfig.PrefetchCount` 注入（默认 32 或 64）。
- **触发条件**：Step 15 双实例上线后立即（已上线）；like_consumer / pv_consumer / count_consumer 三个 Consumer 都受影响。
- **重构成本**：S（一处 Qos 调用 + 一项 Config）
- **关联 ADR**：PRD v3.0 §3.3
- **关联偏离点**：无
- **建议处置**：**立即修**（档 1）。本批之后单独提小 PR 修。

### TD-MQ-04 · reconnectDelay 上限 30s 硬编码

- **档位**：1
- **代码锚点**：`internal/event/rabbitmq.go:reconnectDelay:202-209`（`const maxDelay = float64(30 * time.Second)`）
- **现状**：虽然 `ReconnectInitialDelay` 来自 Config，但 cap 上限 30s 写死，违反 §七 Config-First；如要拉长 backoff 周期需改代码。
- **理想**：`MQConfig.ReconnectMaxDelay`，default=30s。
- **触发条件**：生产 RabbitMQ 集群故障窗口超 30s 时，重连风暴持续 5 次 × 30s = 150s 后耗尽 attempt 退出；想拉长窗口需重新构建镜像。
- **重构成本**：S（< 1h）
- **关联 ADR**：conventions §1.1（Config-First）
- **关联偏离点**：无
- **建议处置**：与 TD-EVT-03 / TD-CFG-02 合并一次 PR。

### TD-ERR-01 · Post 模块错误码段位 412xxx 与 PRD §7 标称 420xxx 不一致

- **档位**：1
- **代码锚点**：`pkg/errcode/errcode.go:71-82`（`ErrTitleEmpty=412101` 等 8 条）+ `pkg/errcode/errcode.go:5`（doc 注释里写 `12=post`）
- **现状**：实现层段位 `412xxx`；CLAUDE.md §六 "错误码段位" 表里写"内容 42YZZZ"；PRD v3.0 §7 表里若沿用 CLAUDE.md 也会标 420xxx。文档与实现冲突。
- **理想**：对齐——以实现为准，把 CLAUDE.md §六 + PRD v3.0 §7 的 "42" 改为 "412"；并同步规范段位文档为 `XYYZZZ`（X+YY = 412）。
- **触发条件**：新人按文档分配段位，下一个模块编号撞车；客户端 SDK 自动生成错误码常量表时分类错误。
- **重构成本**：S（文档修订 + git grep `42[0-9]xxx` 全局检查；不动代码）
- **关联 ADR**：CLAUDE.md §六 / PRD v3.0 §7
- **关联偏离点**：本批新发现
- **建议处置**：**立即修文档**（档 1，但只动 .md），并写进 PRD v3.0 errata 章节。

### TD-ERR-02 · Encryption 错误码标称"never returned to HTTP"，实际被 service 层返回

- **档位**：1
- **代码锚点**：`pkg/errcode/errcode.go:11-13` + `pkg/errcode/errcode.go:120-127`（doc 注：`Audit (470xxx) and Encryption (480xxx) codes are never returned to HTTP callers`）；实际返回点见 `internal/service/user_service.go` 中 `ErrEncryptPhone` 的 return（grep 已确认存在）
- **现状**：注释与代码不一致 —— `ErrEncryptPhone` 的 HTTPStatus=500，且在 user_service 出错路径里被 return 给 handler → response 中间件序列化为 HTTP 500 + body `{"code":480101,...}` 给客户端。
- **理想**：二选一：(a) 删除注释，承认 480xxx 会作为 500 错误码暴露；或 (b) 在 service 层把 ErrEncryptPhone 包装为 ErrServer 后再返回，注释保留。从安全暴露面看 (b) 更稳妥。
- **触发条件**：客户端遇到加密失败时收到 480101 而非 500001 → 暴露内部模块边界；安全审计阶段会标红。
- **重构成本**：S（service 层一处包装 + 单测）
- **关联 ADR**：PRD v3.0 §11.1 / Step 11 偏离点
- **关联偏离点**：Step 11 偏离点登记中应有
- **建议处置**：选 (b)。本批后单独 PR。

### TD-CFG-01 · ConsumerConfig 三个子结构字段重复，没有共用基类

- **档位**：4
- **代码锚点**：`pkg/config/config.go:LikeConsumerConfig:170` / `PVConsumerConfig:175` / `CountConsumerConfig:180`（都是 BatchSize + FlushInterval）
- **现状**：三个结构体字段完全相同；未来加 Audit / Search Consumer 时仍需复制。
- **理想**：定义 `BatchConsumerConfig{BatchSize, FlushInterval}`，三个 sub-config 嵌入或直接复用。
- **触发条件**：增加 Audit/Encryption/Cleanup Consumer 时；目前 3 个尚可忍。
- **重构成本**：S（< 1h，注意 mapstructure tag 兼容）
- **关联 ADR**：conventions §1.1
- **关联偏离点**：无
- **建议处置**：等第 4 个 Consumer 出现时再合。

### TD-CFG-02 · MQ.URL 在 Provider=rabbitmq 时未做 validate

- **档位**：1
- **代码锚点**：`pkg/config/config.go:validate:357-367`（switch 上只校验 Provider 取值，未校验 URL）
- **现状**：`mq.provider=rabbitmq` 但 `mq.url=""` 启动不报错；`NewRabbitMQBus` 后续 dial 会失败，但报错信息是 `dial: open tcp: ...`，不指向"配置缺失"。
- **理想**：与 OSS/SMS provider=aliyun 时校验 AccessKey 一致 —— 加 `if cfg.MQ.Provider == "rabbitmq" && cfg.MQ.URL == "" { return error }`。
- **触发条件**：误删 config.prod.yaml 中 mq 段 URL 时启动失败信息不够直观；新人 onboarding 排查难。
- **重构成本**：S（一行 if）
- **关联 ADR**：conventions §1.1 / B5 启动期校验
- **关联偏离点**：无
- **建议处置**：本批后小 PR 修。

### TD-CFG-03 · validate 缺失：Log/Redis/Database/Metrics/Encryption.PhonePepper/Audit.Region

- **档位**：1
- **代码锚点**：`pkg/config/config.go:validate:337-498`（仅校验 Server/Database.DSN/Redis.Addr/JWT/MQ/SMS/Audit Provider/OSS/Ratelimit/Consumer/Cache/Encryption.PhoneKey）
- **现状**：
  - `Log.Level` 未校验取值集合（debug/info/warn/error），写 `"DEBUG"` 启动也通过
  - `Redis.PoolSize` 未校验 > 0
  - `Database.MaxOpenConns/MaxIdleConns` 未校验范围
  - `Metrics.Namespace` 未校验非空
  - `Encryption.PhonePepper` 完全未校验（prod 必须非空，避免 hash 撤回 pepper）
  - `Audit.Region` 未校验已知 region 集合
- **理想**：在 validate 末尾补 6 条 if；与 Encryption.PhoneKey 既有逻辑保持同风格。
- **触发条件**：生产配置漂移时启动期不报错，运行期间接报错（如 Redis 池耗尽 / metrics 命名空间空致 collector panic）。
- **重构成本**：S（半天，含单测）
- **关联 ADR**：conventions §1.1 / 体检 M1
- **关联偏离点**：无
- **建议处置**：本批后小 PR 修。

### TD-CACHE-01 · MQConfig 缺 publisher 侧 confirm/timeout 关键参数

- **档位**：1
- **代码锚点**：`pkg/config/config.go:MQConfig:78-84`；`internal/event/rabbitmq.go:Publish:214-268` 中 `PublishWithContext(... false, false, ...)`（mandatory=false / immediate=false）
- **现状**：mandatory=false 意味着 broker 找不到 binding 时**静默丢弃**；无 Publish Confirms（amqp091 的 NotifyPublish）→ Publish 返回 nil 不代表消息已持久化。
- **理想**：开启 Confirm 模式（broker 持久化后才返回 ack）；至少把 mandatory 提升为 config 选项；alert on returned message。
- **触发条件**：bind 出错 / queue 被运维误删后，业务层认为发布成功但消息已丢；Step 13 真切 RabbitMQ 后立刻成为隐患。
- **重构成本**：M（Confirm 模式重构 publish 路径 + 测试）
- **关联 ADR**：PRD v3.0 §3.3 / 体检 §3
- **关联偏离点**：无
- **建议处置**：Step 18 上线前必须修；当前生产期 PV/Count 漂移可容忍，但 Like/Follow 已临近核心数据，建议优先。

### TD-SMS-01 · NewSender 文档注释陈旧（"Step 10 将接 aliyun"，实际已接）

- **档位**：4
- **代码锚点**：`pkg/sms/sender.go:25-30`（`Provider="aliyun" will be wired up in Step 10`）
- **现状**：注释停留在 Step 9 时期，实际 case "aliyun" 已 return NewAliyunSender；属于文档腐烂。
- **理想**：删掉过时句子，改为简单陈述 provider 行为。
- **触发条件**：新人误读注释以为 aliyun 未接；不会引发线上事故，但属"小窗效应"。
- **重构成本**：S（5 分钟）
- **关联 ADR**：无
- **关联偏离点**：无
- **建议处置**：与下一次 sms 包修改 piggyback 改。

### TD-SMS-02 · Aliyun TemplateParam 用 fmt.Sprintf 拼 JSON，code 含 `"` 即破坏 payload

- **档位**：1
- **代码锚点**：`pkg/sms/aliyun.go:SendCode:60`（`req.TemplateParam = fmt.Sprintf(\`{"code":"%s"}\`, code)`）
- **现状**：code 当前由 `pkg/validator` 生成（纯数字），看似安全。但接口契约要求"phone format is assumed validated"，code 字段无 contract；任何"生成方接受非数字 code"的变更（如字母短信验证码）会导致 Aliyun API 收到非法 JSON 或注入。
- **理想**：用 `encoding/json.Marshal(map[string]string{"code": code})`。
- **触发条件**：未来扩展支持图形验证码 / 营销短信时引入非数字 code 即触发。
- **重构成本**：S（3 行替换 + 单测）
- **关联 ADR**：CLAUDE.md §九（防御深度）
- **关联偏离点**：无
- **建议处置**：本批后小 PR 修；走"立即修"路线（档 1：违反输入安全自律）。

### TD-SMS-03 · Aliyun Region 硬编码 "cn-hangzhou"，无 config 旁路

- **档位**：2
- **代码锚点**：`pkg/sms/aliyun.go:NewAliyunSender:40`
- **现状**：Region 写死。doc.go 注释自圆其说"SMS 是全局服务"，但同 SDK 在其它 region 的可用性、限流策略并不完全一致；尤其当业务出海后需要落地东南亚或欧洲 endpoint。
- **理想**：进 `SMSConfig.Region`，default=`cn-hangzhou`。
- **触发条件**：出海部署 / 阿里云 cn-hangzhou region 单点故障 → 切 region 需改代码。
- **重构成本**：S（< 1h）
- **关联 ADR**：无（Step 10 决策时未列入 ADR）
- **关联偏离点**：无
- **建议处置**：永久接受亦可；标注"已知架构所限"，与 TD-CFG-02 一起 PR。

### TD-SMS-04 · SendCode 显式丢弃 ctx，无取消传播 / 超时控制

- **档位**：1
- **代码锚点**：`pkg/sms/aliyun.go:SendCode:55`（`func (s *AliyunSender) SendCode(_ context.Context, ...)`）
- **现状**：阿里云 SDK v1（`alibaba-cloud-sdk-go/services/dysmsapi`）的 `SendSms` 不接 ctx；当前实现直接丢 `_ context.Context`。HTTP handler 端 ctx 取消（client 关连接）后，SDK 调用仍在跑。
- **理想**：(a) 切换到支持 ctx 的 SDK v2；或 (b) 用 `context.WithTimeout` + 一个 goroutine + done channel 模拟 ctx 取消，至少能在 timeout 时返回。
- **触发条件**：阿里云接口慢响应（>3s）时，前端等不到响应已关连接，但 Go 仍在等 SDK 返回；高峰期协程堆积。
- **重构成本**：M（SDK 升级或包一层 ctx 适配器，要测试）
- **关联 ADR**：体检 §5.4
- **关联偏离点**：无
- **建议处置**：上线前用 (b) 兜底；SDK v2 迁移做单独 PR。

---

## 本批批注（收口时填写）

- **档位分布**：档 1 = 9（TD-EVT-04 / TD-MQ-03 / TD-MQ-04 / TD-ERR-01 / TD-ERR-02 / TD-CFG-02 / TD-CFG-03 / TD-CACHE-01 / TD-SMS-02 / TD-SMS-04）；档 2 = 2（TD-MQ-01 / TD-SMS-03）；档 3 = 2（TD-EVT-01 / TD-MQ-02）；档 4 = 4（TD-EVT-02 / TD-EVT-03 / TD-CFG-01 / TD-SMS-01）；合计 18 条（注：TD-SMS-04 计为档 1，TD-EVT-04 已计入档 1）
- **关键发现**：
  1. **错误码段位文档腐烂**：实现层 Post 用 412xxx，CLAUDE.md / PRD 表标称 420xxx —— B1 评分卡"全模块段位一致"未达标。
  2. **RabbitMQ 生产配置不完整**：缺 Qos/Prefetch（TD-MQ-03）+ 缺 Publish Confirms（TD-CACHE-01）+ 缺 URL validate（TD-CFG-02），构成"切了 RabbitMQ 但生产语义未对齐"的系统性风险。
  3. **Config-First 在 ChannelBus bufSize / RabbitMQ maxDelay / 拓扑前缀 / SMS Region 上多处遗漏**，违反 conventions §1.1。
  4. **ErrEncryptPhone 注释与代码冲突**：注释承诺"never returned to HTTP"，实际通过 service 层向 client 暴露 480101。
- **新增护栏**：本批未新增（既有 15 条已覆盖；新发现属"应做但未做"，不属"刻意做对"）。
- **下一批衔接点**：
  - Batch 2 审 user/post service 时关注 ErrEncryptPhone 的实际返回点是否需要 wrap（TD-ERR-02 联动）。
  - Batch 3 审 like_consumer / pv_consumer 时关注 handler 错误处理是否需要按 TD-EVT-04 / TD-CACHE-01 的取舍调整。
  - Batch 6 审 deploy/ 时关注 Step 13 切 RabbitMQ 后 publish-confirm / prefetch / DLX 监控（TD-MQ-03 / TD-CACHE-01）。

---

## 本批批注（收口时填写）

- **档位分布**：档 1 = _ / 档 2 = _ / 档 3 = _ / 档 4 = _
- **关键发现**：
- **新增护栏**：
- **下一批衔接点**：
