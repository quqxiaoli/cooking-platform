# 故事线 · 第 13 步 · EventBus 切换 RabbitMQ（生产加固）

---

## Part A · 本步故事线

### 1. 起点：进入本步时的状态

Step 12 结束后，项目完成了日志脱敏和错误码收口，整个第二轮功能补齐也宣告完成。此前在 Step 13 体检阶段（Step 12 结束后、Step 13 正式开工前），已经有一个 MVP 版的 `rabbitmq.go`：用 `autoAck=true`、`DeliveryMode=Transient`、临时匿名队列实现了 ChannelBus 到 RabbitMQ 的"能跑"切换。文档注释里留着明确的 TODO：Step 13 需要把 at-most-once 加固为 at-least-once。

进入本步时的两个核心遗留点：

1. **RabbitMQBus 是 MVP 级别**：消息丢了不重试，broker 重启消息消失，每个 Subscribe 用匿名排他队列，多实例部署时每个实例各收一份（pub-sub 语义），而不是多实例共享消费（point-to-point）。
2. **docker-compose.yml 里没有 RabbitMQ**：dev 环境装基础设施的事还没做。

### 2. 设计思考：三个关键取舍

**取舍一：命名队列 `cooking.<topic>` 还是 `cooking.<topic>.<consumer_group>`**

conventions §1.4 明确规定生产命名应该是 `cooking.<topic>.<consumer_group>`，这样不同消费者组可以各自独立消费同一 topic 的消息。但 `EventSubscriber` 接口是 `Subscribe(ctx, topic, handler)`，没有 consumer_group 参数。改接口意味着要修改 bus.go（导出符号 EventSubscriber）+ 所有 Consumer + ConsumerManager，约束散布了整个 consumer 层。

我在开工假设清单阶段提出了这个问题，用户选择了方案 A：只用 `cooking.<topic>`。理由是：这个项目只有一套应用，不存在多个不同消费者组消费同一 topic 的需求。多个应用实例竞争同一队列，恰好是我们想要的负载均衡效果。这是一个"偏离规范但语义正确"的决策，已登记为偏离点（PRD v3.0 §3 EventBus 章节更新时需体现）。

**取舍二：DLX 重试策略**

conventions 里说"countBatchSize 次 Nack 后进 DLX"，暗示有重试计数。但 RabbitMQ 经典队列（classic queue）不支持 `x-delivery-count`，要做重试计数要么用 quorum queue（需要更高版本 RabbitMQ 且有选举开销），要么用 Redis 辅助计数（引入额外依赖和原子操作）。

分析了现有 Consumer 的 handler 错误路径：
- **LikeConsumer/PVConsumer/CountConsumer**：handler 只把事件 push 到内部 channel，几乎不会返回 error，真正的错误在 flush 里处理（已有重试日志逻辑）。
- **AuditConsumer**：已经是 fail-closed 设计，API 失败不翻转帖子状态，重跑没有意义（重跑只会再次失败或产生重复 audit_log）。

结论：handler error → 直接 DLX（no-retry），poison message 不会堵死队列，真正需要重试的逻辑已经在 flush/handler 内部处理。复杂度不成比例。

**取舍三：重连架构放在哪一层**

方案 A：重连逻辑内置在 `Subscribe` 的 retry loop 里（`subscribeOnce` 返回 `connectionLost=true` 后退避重连）。  
方案 B：放在 ConsumerManager，Shutdown 后重新 StartAll。

方案 A 更内聚：Consumer 无感知，ConsumerManager 接口不变，重连粒度是"一个 topic 一个重连"而不是"所有 Consumer 整体重启"。唯一需要小心的是：多个 Subscribe goroutine 同时检测到连接断开时，`ensureConnLocked` 必须保证只有一个 goroutine 真正 redial（其他等待者用新连接）。用 `b.conn.IsClosed()` 检查 + mutex 保护实现了这个语义。

### 3. 实现节奏

**第一步**：Config-First 先行。在 `MQConfig` 里追加 `ReconnectMaxRetries` 和 `ReconnectInitialDelay`，同时把 `NewRabbitMQBus` 签名从 `(url, timeout)` 改为接受整个 `config.MQConfig`。这是 Step 13 起要求的 Config-First 规范落地：所有可调参数必须进 config，不许零散传参。

**第二步**：改写 `rabbitmq.go`。核心逻辑拆成三层：
- `dialAndDeclareLocked`：建连 + 声明拓扑（idempotent，reconnect 时可重复调用）
- `ensureConnLocked` / `reopenPubChLocked`：懒检查，多 goroutine 安全
- `subscribeOnce`：单次订阅会话，返回 `(connectionLost bool, err)`，上层 retry loop 决定是否重连

DLX 拓扑：main exchange（topic）+ DLX exchange（fanout）+ DLX 队列（catch-all）三者在 `declareTopology` 里声明，每次 `dialAndDeclareLocked` 都调用，保证幂等。

**第三步**：docker-compose.yml 追加 RabbitMQ 服务，`make docker-up` 验证 Healthy。

**第四步**：verify_step13.sh 开发过程中遇到了两个问题：

1. **bash 3.2 + `$!` 问题**：`go run ./cmd/server 2>&1 | tee -i log &` 后的 `$!` 在 bash 3.2 里有边界问题（pipe 后的 `$!` 可能未设置），改为 `go run ... > log 2>&1 &` 解决。

2. **pipefail + grep -q SIGPIPE 问题**：`rabbitmqctl list_exchanges | grep -q "pattern"` 在 `set -euo pipefail` 下总是返回 NOT_FOUND。原因是：`grep -q` 找到第一个匹配后立即退出，`rabbitmqctl` 的 pipe write 端收到 SIGPIPE，exit code 变为 141；`pipefail` 把整个 pipeline 判定为失败，`&&` 没有执行，走了 `||` 的 fail 分支。修复：先 `rabbitmqctl > tmpfile`，再 `grep -q tmpfile`，避免 pipe 提前关闭。

### 4. 踩坑记录

**坑 1：验证时忘记关旧 server**

verify_step13.sh 第一次运行时，port 8080 已有旧 server 在跑（之前 make run 留的），health check 命中了旧 server（channel provider），导致脚本继续但 exchange 未声明。Fix：cleanup 函数里用 `lsof -ti :8080` 同时杀掉 go run wrapper 和实际 server binary（两个不同 PID）。

**坑 2：go run 产生两个进程**

`go run ./cmd/server &` → `$!` 是 go run 进程的 PID，但实际监听端口的是 go run 编译后 spawn 出的 server binary，PID 不同。`kill $SERVER_PID` 只杀了 go run，server binary 成为孤儿继续跑。Fix：cleanup 里补一个 `lsof -ti :8080` 的额外 kill。

**坑 3：post_id 字段路径**

jq 路径写成了 `.data.post.id`，实际 DTO 字段是 `.data.post_id`（扁平结构，不嵌套 post 对象）。这是不看代码直接写脚本的典型错误，应该先读 handler/dto 确认响应结构。

### 5. 收获与伏笔

**理解提升**：

- RabbitMQ at-least-once 的关键不是"不丢消息"（持久化解决这个），而是"消费后确认"——只有 handler 成功后才 Ack，进程崩溃时 broker 会重新投递。这个语义和 Channel 模式的最大差异是：消息可能被消费两次，Consumer 必须幂等。LikeConsumer 的 `INSERT IGNORE` 和 `GREATEST(0, count-delta)` 设计在 Step 5 就埋下了这个伏笔，现在 at-least-once 来了，它已经是幂等的。
- `pipefail` + `grep -q` 的 SIGPIPE 问题是 CI 脚本中的经典陷阱，值得记住。
- bash 3.2（macOS 默认）和 bash 5.x 在 `$!`、数组、字符串处理上有多处差异，macOS 下写脚本要格外注意。

**Step 14 伏笔**：

Step 14 要做 MySQL 主从复制 + DBResolver 读写分离。目前所有 Consumer 都用同一个 `*gorm.DB`，Step 14 引入 DBResolver 后，`db.WithContext(ctx)` 会根据操作类型（SELECT/INSERT）自动路由到 slave/master，Consumer 代码理论上无需改动——但需要验证 Consumer 的 `SELECT ... FOR UPDATE` 等锁操作是否被正确路由到 master。这是 Step 14 开工前需要检查的点。

**PRD v3.0 §3 EventBus 章节需更新**：
- 补充 at-most-once（ChannelBus）→ at-least-once（RabbitMQBus）演进路径
- 补充命名队列约定（`cooking.<topic>` 及偏离原因）
- 补充 DLX 拓扑和 dead-letter 处理策略

---

## Part B · 关联知识点清单

**基础概念（语言 / 框架层面）**

- AMQP 0-9-1 协议基础：exchange、queue、binding、routing key、consumer tag
- RabbitMQ 消息确认机制：autoAck vs. manual ACK/Nack，requeue 语义
- RabbitMQ 持久化：exchange durable、queue durable、DeliveryMode=Persistent 三者缺一不可
- 死信队列（DLX）：x-dead-letter-exchange 参数、fanout exchange catch-all 模式

**设计模式与架构思想**

- at-most-once / at-least-once / exactly-once 消息语义对比
- 幂等消费设计（INSERT IGNORE、GREATEST(0,…)）
- Poison message 防治：不无限重试，直接 DLX
- Config-First 模式：可调参数全部进 config，避免"deploy 才能调参"

**Go 语言特性与并发模型**

- sync.Mutex 保护共享 AMQP channel（amqp.Channel 非 goroutine-safe）
- atomic.Bool 实现 closed 状态的无锁读（读热路径）
- 函数返回多值 `(bool, error)` 编码 reconnect/clean-shutdown 两种退出原因
- 指数退避：`math.Pow(2, float64(attempt-1))` + Duration 类型转换

**工程实践（测试 / 部署 / 可观测性等）**

- Docker Compose healthcheck 与 `depends_on: condition: service_healthy` 模式
- bash 3.2 兼容性：`$!` 在 pipeline 后的行为、`set -euo pipefail` + `grep -q` 的 SIGPIPE 陷阱
- `go run` 产生两个进程（wrapper + binary）的进程管理问题
- 验证脚本中临时文件 (`mktemp`) 替代 pipe 避免 SIGPIPE

**数据安全与合规**

- RabbitMQ 连接串含密码，config.prod.yaml 必须用 `<env:APP_MQ_URL>` 占位

**可能被延伸追问的关联领域**

- RabbitMQ quorum queue vs. classic queue（x-delivery-count、更强一致性）
- Kafka vs. RabbitMQ：消费模型差异（pull vs. push、offset vs. ack）
- GORM DBResolver 读写分离与 Consumer 中的 SELECT/INSERT 路由（Step 14 预热）
