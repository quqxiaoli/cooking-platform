# 故事线 · 第 16 步 · Prometheus + Grafana 监控

---

## Part A · 本步故事线

### 起点：Step 15 之后留下的"黑盒"

进入 Step 16 时，双实例 + Nginx 的架构已经跑起来了，verify_step15.sh 的 §5 也确认了 100 次请求均匀打到 app1 和 app2。但有个令人不安的现实：从外部看，整个系统就像一个黑盒——我知道它"活着"（health/ready 返回 200），但不知道它"活得怎么样"。

PRD v2.2 §11 描述了可观测性体系，conventions §2 列出了 P0/P1/P2 优先级的指标清单。本步的核心任务是让这个黑盒变透明。

### 设计思考一：metrics 包怎么组织

第一个决策是指标定义放哪里。有两个方向：

**方向 A**：各模块就地注册，middleware 里注册 HTTP 指标，consumer 里注册 consumer 指标，散落各处。

**方向 B**：统一放在 `pkg/metrics/` 包，Init(namespace) 一次性注册所有指标。

选 B 的理由很实际——如果散落各处，不同 init() 的调用顺序不可控；而更重要的是，方向 A 在测试时会遇到"重复注册 panic"的问题（prometheus.MustRegister 在同进程第二次调用会 panic）。统一用 Init() 函数，调用方可以控制是否初始化，不调用 = 所有指针为 nil = 各埋点 nil-guard 跳过，测试环境零侵入。

namespace 从 `cfg.Metrics.Namespace` 注入，这是 Config-First 规则的自然延伸——"cooking" 是默认值，但如果未来多服务共享同一 Prometheus 实例，可以改名区分。

### 设计思考二：MySQL Pool 指标用哪种姿势

P1 指标里有"MySQL 连接池使用率"。conventions §2 写的是"GORM callback"，但实际上 GORM callback 是针对单次查询的钩子（BeforeQuery / AfterQuery），而连接池使用率是池级别的聚合状态，并不是某一次查询的属性。

用 GORM callback 能拿到"这次查询花了多久"，但拿不到"池里现在有多少空闲连接"。后者必须通过 `db.DB().Stats()` 读取 `sql.DBStats`。

这里有两种实现方式：
1. **Push 模式**：起一个 goroutine，每 N 秒读一次 Stats，写入 Gauge。
2. **Pull 模式**：实现 `prometheus.Collector` 接口，Prometheus scrape 时 Collect() 被调用，实时读 Stats。

拉模型天然和 Prometheus 架构对齐——Prometheus 本身就是拉取式的，让采集者主动驱动，数据永远是抓取时刻的最新值，不存在"采集间隔不同步"的问题，也不需要额外 goroutine。于是登记了一个偏离点：实现从"GORM callback"改为"sql.DB pull-based Collector"，理由充分，性质上不算偏离精神只是偏离了实现路径。

### 设计思考三：Redis Hook 的装饰器模式

go-redis v9 的 Hook 接口有三个方法：DialHook / ProcessHook / ProcessPipelineHook。ProcessHook 是最有价值的——每次 GET/SET/ZADD 等命令调用都会经过它，可以在 next(ctx, cmd) 前后包一层计时。

这是一个教科书级的装饰器模式：Hook 拿到 next（原始执行函数），返回一个新函数，新函数在调用原函数前后加了计时逻辑，原函数本身毫不知情。这种"非侵入式插桩"的思路在面试中很值得展开——它同样适用于 gRPC interceptor、HTTP middleware、数据库 driver 层。

一个细节：redis.Nil 不是真正的错误（"key 不存在"是正常业务语义），所以 status 标签的判定是 `err != nil && !errors.Is(err, redis.Nil)`。如果把 Nil 也算 error，所有的缓存 miss 都会被记成 error，让错误率指标完全失真。

### 实现节奏：从 pkg 到 edge

按照项目分层习惯，从最里层开始往外写：

1. 先写 `pkg/metrics/` 三件套（metrics.go / redis_hook.go / mysql_collector.go）——纯库代码，不依赖任何业务模块。
2. 改 `internal/middleware/metrics.go`——这个文件 Step 2 就已经占了一个空壳，就等这一步填实。
3. 改 `cmd/server/main.go`——按启动顺序插入：metrics.Init 在 Logger 后、MySQL 前；MySQLCollector 在 MySQL 初始化后；RedisHook 在 Redis 初始化后；/metrics 路由在 setupRouter 内。
4. 改三个 Consumer——最轻量的改动，subscribe goroutine 里各加一行 Inc()，ticker case 里各加一行 queue depth Set()。CountConsumer 的 `send()` 方法加了一个 `topic string` 参数（因为 CountConsumer 订阅了 5 个 topic，需要分别打标签）。
5. 部署层——prometheus.yml + Grafana provisioning + docker-compose.yml 追加两个服务。

### 踩坑记录

验证脚本第一次跑失败了（§2 找不到 cooking_http_requests_total），原因很直接：app1/app2 在 Step 15 就已经 build 进镜像了，新代码没有编译进去。`docker compose build app1 app2` 重建后问题消失。

这个坑值得记一下——Docker 镜像层缓存在本地开发中是双刃剑。代码改了但镜像没重建，验证失败，第一反应可能会往代码里找问题。以后 verify 脚本可以考虑加一步"触发重建"或"检查镜像构建时间戳"。

### 收获

1. **可观测性的三种"接入姿势"** 在这一步同时出现了：
   - Gin middleware（侵入式，但在框架层统一接入，业务代码零感知）
   - go-redis Hook（装饰器，非侵入式，库级别的钩子）
   - sql.DB Collector（pull-based，数据库连接池的拉取式采集）
   三种姿势各有适用场景，现在可以在面试中对比着讲。

2. **Grafana provisioning** 让 Dashboard 变成了代码（IaC 思路）。不再需要手动导入 JSON，容器启动自动就绪，可以纳入 Git 版本管理，Review 时也能 diff dashboard 变化。

3. **Prometheus 抓取双实例**的好处在 §4 验证时体现出来了：targets up: 2，两个实例各自有独立的 instance 标签，GC pause、goroutine 数量等 runtime 指标可以分实例对比，方便排查"app1 有问题而 app2 正常"的场景。

**PRD v3.0 §11 需更新**：监控指标全表、SLO 目标值（P95 延迟目标、Consumer 积压告警阈值）需在 Step 17 生成 PRD v3.0 时补充。

---

## Part B · 关联知识点清单

**基础概念（语言 / 框架层面）**

- Prometheus 数据模型（Counter / Gauge / Histogram / Summary）
- Histogram bucket 设计原则（分位数精度 vs 存储开销）
- `promhttp.Handler()`、`prometheus.DefaultRegisterer` 工作机制
- go-redis v9 `Hook` 接口定义

**设计模式与架构思想**

- 装饰器模式（Decorator）在 middleware / hook 层的应用
- Pull vs Push 两种指标采集模型的适用场景
- IaC（基础设施即代码）思路在 Grafana provisioning 中的体现
- 可观测性三支柱：Metrics / Logs / Traces（本步覆盖 Metrics，Logs 在 Step 12 已做）

**Go 语言特性与并发模型**

- `prometheus.Collector` 接口（Describe + Collect）
- 接口作为"行为抽象"（Hook 接口 vs 直接 monkey-patch）
- nil pointer guard 用于懒初始化场景

**工程实践（测试 / 部署 / 可观测性等）**

- Docker 镜像缓存与代码变更的关系（重建 vs 重启）
- Grafana dashboard as code（provisioning YAML + JSON）
- Prometheus scrape target 配置（static_configs vs service discovery）
- `sql.DB.Stats()`：OpenConnections / InUse / Idle / WaitCount 含义

**数据安全与合规**

- `/metrics` 端点不对外暴露（内网 Prometheus 直连，Nginx 不代理）

**可能被延伸追问的关联领域**

- OpenTelemetry（OTel）与 Prometheus 的关系，未来迁移路径
- Histogram vs Summary 的区别（为什么 Prometheus 官方推荐 Histogram）
- Alertmanager 与告警规则（SLO breach → 触发 PagerDuty / 飞书）
- PromQL 基础：rate() / histogram_quantile() / sum by() 语义
