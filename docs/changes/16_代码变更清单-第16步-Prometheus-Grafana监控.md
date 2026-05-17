# 代码变更清单 · 第 16 步 · Prometheus + Grafana 监控

---

## 基础设施层（新增）

- `pkg/metrics/metrics.go`（新增）
  · 接口：`Init(namespace string)`
  · 指标：`HTTPRequestsTotal` / `HTTPRequestDuration` / `ConsumerProcessedTotal` / `ConsumerQueueDepth` / `RedisCommandDuration`（共 5 个，均为包级指针）
  · 设计要点：Init() 前所有指针为 nil，各埋点均有 nil-guard，测试环境不调用 Init() 时零侵入。namespace 注入让 metric 前缀可配置，Config-First 规则的体现。

- `pkg/metrics/redis_hook.go`（新增）
  · 类型：`RedisHook`，实现 `redis.Hook` 接口（DialHook / ProcessHook / ProcessPipelineHook）
  · 设计要点：装饰器模式，ProcessHook 在 next(ctx, cmd) 前后包计时逻辑。`redis.Nil` 不算 error（key 不存在是正常业务语义，计入 error 会让错误率失真）。Pipeline 命令不单独计时（共享一次 round-trip，per-cmd 延迟样本无意义）。

- `pkg/metrics/mysql_collector.go`（新增）
  · 类型：`MySQLPoolCollector`，实现 `prometheus.Collector` 接口（Describe + Collect）
  · 指标：open_connections / inuse_connections / idle_connections / wait_total（4 个 Desc）
  · 设计要点：pull-based，Prometheus 每次 scrape 触发 Collect()，实时读 `sql.DB.Stats()`。相比 goroutine + Gauge push，无需管理生命周期，数据永远是抓取时刻的最新值。

---

## 配置层（修改）

- `pkg/config/config.go`（修改）
  · 追加 `MetricsConfig{Enabled bool, Namespace string}`
  · `registerDefaults`：`metrics.enabled=true`、`metrics.namespace="cooking"`
  · 设计要点：Enabled 允许在不需要 Prometheus 的环境跳过 Init() 和 /metrics 路由；Namespace 是 PRD v3.0 §11 SLO 指标名的基础。

- `configs/config.yaml`（修改）
  · 追加 `metrics: {enabled: true, namespace: cooking}` 段

- `configs/config.docker.yaml`（修改）
  · 追加同上（Docker 环境与 dev 一致）

---

## 接入层（修改）

- `internal/middleware/metrics.go`（修改，原为空壳）
  · 函数：`Metrics() gin.HandlerFunc`
  · 设计要点：注册在 Recovery() 之后，panic-recovered 请求的 status code 已被设为 500，不会被记成 0。使用 `c.FullPath()` 而非 `c.Request.URL.Path`，路由参数（`:id`）不展开，label cardinality 有界。status 用 class 字符串（"2xx"/"4xx"/"5xx"），不用裸数字。

- `cmd/server/main.go`（修改）
  · 2.5 节：`metrics.Init(cfg.Metrics.Namespace)`（Logger 后、MySQL 前）
  · 3.5 节：`prometheus.MustRegister(metrics.NewMySQLPoolCollector(...))`（MySQL 初始化后）
  · 4.5 节：`rdb.AddHook(metrics.NewRedisHook())`（Redis 初始化后）
  · setupRouter：新增 `metricsEnabled bool` 参数，注册 `middleware.Metrics()` 全局中间件和 `GET /metrics` 路由
  · 设计要点：/metrics 路由条件注册（Enabled=false 时不暴露），对外的 Nginx 不代理 /metrics，保持内网隔离。

---

## Consumer 层（修改）

- `internal/consumer/like_consumer.go`（修改）
  · TopicLike subscribe goroutine：成功 push 到 eventCh 后 `ConsumerProcessedTotal{consumer="like-consumer", topic="event.like"}.Inc()`
  · TopicUnlike subscribe goroutine：同上，topic="event.unlike"
  · flushLoop ticker case：`ConsumerQueueDepth{consumer="like-consumer"}.Set(float64(len(eventCh)))`
  · 设计要点：在订阅 goroutine 计数（而非 flush 时），记录的是"进入批次队列的事件数"，与实际 MySQL 写入数解耦——flush 可能因 duplicate 丢弃事件，但吞吐量指标应反映上游流量。

- `internal/consumer/pv_consumer.go`（修改）
  · 同上模式：subscribe goroutine 计数（topic="event.pv"），ticker 更新 queue depth

- `internal/consumer/count_consumer.go`（修改）
  · `send()` 方法增加 `topic string` 参数（CountConsumer 订阅 5 个 topic，需分别打标签）
  · 5 个订阅 goroutine 各传入对应 topic 常量
  · flushLoop ticker：更新 queue depth
  · 设计要点：`send()` 改签名是本步唯一的"接口变更"，但 send 是私有方法，不影响外部契约。

---

## 部署层（新增 / 修改）

- `deploy/prometheus/prometheus.yml`（新增）
  · scrape job：cooking-platform，targets: [app1:8080, app2:8080]，15s 间隔
  · 设计要点：直连 app1/app2 内网地址，绕过 Nginx，per-instance runtime 指标可分别查询。

- `deploy/grafana/provisioning/datasources/prometheus.yml`（新增）
  · 自动 provision Prometheus 数据源（uid="prometheus"，isDefault=true）

- `deploy/grafana/provisioning/dashboards/provider.yml`（新增）
  · 声明 dashboard 文件目录 /var/lib/grafana/dashboards，30s 更新间隔

- `deploy/grafana/dashboards/cooking-platform.json`（新增）
  · 6 个 panel：HTTP RPS / HTTP P50/P95/P99 / Consumer 处理速率 / Consumer 积压 / Redis P95 延迟 / MySQL 连接池
  · 设计要点：dashboard as code，纳入 Git 版本管理，容器启动自动就绪，无需手动操作 Grafana UI。

- `docs/grafana/cooking-platform.json`（新增）
  · 同上，conventions §2 规定的文档存档位置

- `docker-compose.yml`（修改）
  · 追加 `prometheus`（prom/prometheus:v3.4.0，port 9090）和 `grafana`（grafana/grafana:12.0.1，port 3000）两个服务
  · 追加 `prometheus-dev-data` 和 `grafana-dev-data` 两个 volume

---

## 收尾（新增 / 修改）

- `scripts/verify_step16.sh`（新增）
  · §1 编译检查 / §2 /metrics 包含 4 类指标 / §3 Prometheus healthy / §4 targets up: 2 / §5 Grafana healthy / §6 Prometheus 查询成功
  · 6 段全通过

- `Makefile`（修改）
  · 追加 `verify-step16` target + .PHONY 列表
