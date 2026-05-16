# 代码变更清单 · 第 13 步 · EventBus 切换 RabbitMQ（生产加固）

---

## Config 层

- `pkg/config/config.go`（修改）
  · `MQConfig` 追加两个字段：`ReconnectMaxRetries int`（断线重连最大次数，默认 5）、`ReconnectInitialDelay time.Duration`（指数退避基数，默认 1s）
  · `registerDefaults()` 追加对应默认值
  · `validate()` 追加非负 / 正值校验
  · 设计要点：遵守 Config-First 规则——重连参数属于"不同环境/负载下需要调整的数值"，必须进 config 而非写成 NewRabbitMQBus 的入参魔法数字；同时将 `NewRabbitMQBus` 签名从 `(url, timeout)` 改为接受整个 `config.MQConfig`，避免调用方手动提取字段，接口更内聚

---

## EventBus 层

- `internal/event/rabbitmq.go`（完整改写）
  · **Wire 拓扑**：
    - Main exchange `cooking.events`（topic, durable）：不变
    - DLX exchange `cooking.events.dlx`（fanout, durable）：新增，接收所有死信
    - DLX queue `cooking.events.dlx.queue`（durable）：新增，catch-all，绑定到 DLX exchange
    - 业务队列 `cooking.<topic>`（durable, exclusive=false, x-dead-letter-exchange=DLX）：替换原来的临时匿名排他队列
  · **交付语义**：`autoAck=false` + `DeliveryMode=Persistent`，实现 at-least-once；handler 成功 `Ack(false)`，失败 `Nack(false, false)` 直接进 DLX（避免 poison message 无限重排）
  · **重连架构**：
    - `dialAndDeclareLocked`：建连 + 声明拓扑，idempotent，reconnect 路径复用
    - `ensureConnLocked`：`IsClosed()` 判断 + 按需 redial，多 goroutine 安全（mu 保护）
    - `reopenPubChLocked`：先尝试从现有 conn 开新 channel，失败则 full redial
    - `Subscribe` retry loop：`subscribeOnce` 返回 `(connectionLost bool, err)`，配合指数退避重连
  · **命名队列**：`queueName(topic) = "cooking." + topic`，多实例竞争同一队列 = 负载均衡（偏离 conventions §1.4 的 consumer_group 后缀，见偏离点说明）
  · **指数退避**：`ReconnectInitialDelay × 2^(attempt-1)`，硬上限 30s

- `cmd/server/main.go`（修改）
  · `initEventBus` 中 `NewRabbitMQBus(cfg.URL, cfg.Timeout)` 改为 `NewRabbitMQBus(cfg)`
  · 设计要点：调用方透传整个 MQConfig，符合 Config-First 约定，后续新增 MQ 参数时调用处无需修改

---

## 基础设施层

- `docker-compose.yml`（修改）
  · 新增 `rabbitmq` 服务：`rabbitmq:3.13-management-alpine`，端口 5672（AMQP）+ 15672（Management UI）
  · 环境变量：`RABBITMQ_DEFAULT_USER=cooking / RABBITMQ_DEFAULT_PASS=cooking123`
  · healthcheck：`rabbitmq-diagnostics ping`，确保 `make docker-up` 幂等等待就绪
  · 新增 `rabbitmq-dev-data` volume，跨重启保持队列/exchange 状态
  · 设计要点：management plugin 在 dev 环境很有价值——RabbitMQ Management UI 可以实时查看 DLX 队列里的死信，是生产问题排查的利器

- `configs/config.yaml`（修改）
  · `mq` 段追加 `reconnect_max_retries: 5` 和 `reconnect_initial_delay: 1s`
  · 注释说明了指数退避的实际效果（1s→2s→4s…上限 30s）

- `configs/config.prod.yaml`（修改）
  · `mq` 段完整更新：`provider: rabbitmq`，`url: "<env:APP_MQ_URL>"`（占位符，不 commit 真实密码），`timeout: 10s`（prod 网络延迟放宽），追加重连字段
  · 设计要点：config.prod.yaml 作为生产部署参考文档，每个字段都有注释说明"为什么和 dev 不同"，这是 conventions §4.1 的要求

---

## 验证层

- `scripts/verify_step13.sh`（新增）
  · §1 编译检查 / §2 RabbitMQ 健康 / §3 server 启动（rabbitmq provider）
  · §4 Exchange 拓扑 / §5 DLX Queue / §6 at-least-once 集成（like→MySQL→unlike→MySQL）/ §7 命名队列
  · 工程细节：
    1. `rabbitmqctl | grep -q` 改为先写 tmpfile 再 grep，避免 bash 3.2 `pipefail` + SIGPIPE 误判
    2. cleanup trap 额外通过 `lsof -ti :8080` kill 实际 server binary（go run 产生两个进程）
    3. `go run ... > log 2>&1 &` 替代 `go run ... | tee log &` 避免 pipeline `$!` 边界问题

- `Makefile`（修改）
  · 追加 `verify-step13` 目标
  · `.PHONY` 列表追加 `verify-step13`
