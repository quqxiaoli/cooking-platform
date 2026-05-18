# Batch 6 · 生产基础设施（Step 13–17）

> **审计范围**：
> - **Step 13 RabbitMQ 切换**：`internal/event/rabbitmq.go`（生产视角复审：ACK / DLX / 持久化 / 重连）、`deploy/rabbitmq/`、`configs/config.prod.yaml` 中 mq 段
> - **Step 14 MySQL 主从**：`deploy/mysql/`（master.cnf / slave.cnf / init-slave.sh / init-master.sql）、`pkg/config/config.go` 中 `SlavesDSN`、`docker-compose.yml` mysql-master/slave/slave-init 服务、DBResolver 装配位置
> - **Step 15 Nginx 双实例**：`deploy/nginx/nginx.conf`、`docker-compose.yml` app1/app2/nginx 服务、`internal/handler/health.go`（含 /readiness 与 /health/ready 别名）
> - **Step 16 Prometheus + Grafana**：`pkg/metrics/`（metrics.go / mysql_collector.go / redis_hook.go / doc.go）、`internal/middleware/metrics.go`、`deploy/prometheus/`、`deploy/grafana/`
> - **Step 17 CI/CD**：`.github/workflows/`（pr.yml / build.yml / release.yml）、`internal/integration/ping_test.go`、`scripts/verify_step1[3-7].sh`
> - **跨步辅助**：`configs/config.{yaml,docker.yaml,prod.yaml}` 三套配置差异性审计、`Makefile`、`Dockerfile`
>
> **审计意图**：审"从单机 dev 到双实例 prod"的工程化决策；这里的债大量属档 2（架构所限：开源版 Nginx 无 active health check / Grafana 默认凭据）和档 4（MVP 妥协：无日志聚合 / 无 AlertManager / 无 prod compose）。
>
> **关联材料**：PRD v3.0 §2 / §3.3 / §5.2 / §9 / §10 / §12，progress/13 / 14 / 15 / 16 / 17 偏离点
>
> **与 Batch 1 的边界**：RabbitMQ 客户端层（Publish/Subscribe/DLX/重连）已在 Batch 1 由 TD-MQ-01..04 / TD-EVT-04 完整覆盖；Batch 6 视角下沉到 **deploy 工件 / 编排 / 鉴权边界**，不重复客户端层条目。

---

## 本批条目

### TD-INFRA-01 · 缺 `docker-compose.prod.yml`，prod 编排没有可执行文件

> **状态：✅ 已修复（Step 18 pre-cleanup B1）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：4
- **代码锚点**：
  - `configs/config.prod.yaml:3`：注释明确写 `Loaded when CONFIG_PATH=configs/config.prod.yaml (set in docker-compose.prod.yml).`
  - 仓库根仅 `docker-compose.yml`（dev），无 prod 等价物
- **现状**：prod 仅有 `config.prod.yaml` 模板与 `Dockerfile`，没有 prod compose（mysql master+slave / Redis Sentinel / RabbitMQ 集群 / 双 app / Nginx / Prom / Grafana 的整套 prod 编排）。所谓"上线"完全靠手工拼。
- **理想**：补 `docker-compose.prod.yml`（或 k8s manifest），口令走 `.env`，挂载、网络隔离、健康检查、资源限制一并定义；至少要让 `make docker-prod-up` 能起来。
- **触发条件**：进入 Step 18（部署上线）的第一天，必撞。
- **重构成本**：M（1–3 天，含 Sentinel + RabbitMQ 集群配置抽象）
- **关联 ADR**：PRD §2 / §12 部署架构
- **关联偏离点**：无明确登记（隐性偏离，进度文档把"部署上线"留给 Step 18–20）
- **建议处置**：Step 18 立即修

---

### TD-INFRA-02 · 部署口令全部硬编码，dev 与 prod 没有 `.env` / `env_file` 通道

> **状态：✅ 已修复（Step 18 pre-cleanup B1）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：4
- **代码锚点**：
  - `docker-compose.yml:23` `MYSQL_ROOT_PASSWORD: cooking123`
  - `docker-compose.yml:50` slave 同口令
  - `docker-compose.yml:84-87` slave-init `REPL_PASSWORD: repl123` / `SLAVE_ROOT_PASSWORD: cooking123`
  - `docker-compose.yml:199-200` `RABBITMQ_DEFAULT_PASS: cooking123`
  - `docker-compose.yml:252` `GF_SECURITY_ADMIN_PASSWORD: admin`
  - `deploy/mysql/init-master.sql:9` `IDENTIFIED WITH mysql_native_password BY 'repl123'`
- **现状**：所有 dev compose 口令明文写死在 compose 文件，没有 `env_file: .env`、没有 secrets 通道。当 Step 18 起 prod compose 时，必然按 dev 模式继续硬编码。
- **理想**：dev 用 `.env.dev` + `env_file:`，commit `.env.example` 作模板；prod 通过 Docker secrets / CI 注入 env 变量；compose 内零明文。
- **触发条件**：起 prod compose 时；或团队上 IDE 分享 / 截图意外泄露 dev 仓库。
- **重构成本**：S
- **关联 ADR**：CLAUDE.md §十三（敏感配置走环境变量）
- **关联偏离点**：无
- **建议处置**：Step 18 配 prod compose 时一并接入；dev 可先沿用，但 `.env.example` 该写好

---

### TD-INFRA-03 · `init-master.sql` 用 `mysql_native_password`，MySQL 8.4+ 默认已废弃

- **档位**：2
- **代码锚点**：`deploy/mysql/init-master.sql:9`
- **现状**：repl 用户用 `IDENTIFIED WITH mysql_native_password BY 'repl123'`。MySQL 8.0 兼容，但 8.4 起 `mysql_native_password` 默认不加载（需显式 `--mysql-native-password=ON`），8.x → 9.x 升级时复制链路会瞬断。
- **理想**：用 `caching_sha2_password`（MySQL 8.0+ 默认），同时确认 GTID 复制 + caching_sha2 在拉起 slave 时密码握手 OK。当前老 dump 兼容性问题留到 Step 14 完工后回头改。
- **触发条件**：MySQL 镜像升到 8.4+ / 9.x；或 prod 走 RDS 但 RDS 关掉了 `mysql_native_password`。
- **重构成本**：S
- **关联 ADR**：PRD §5.2 MySQL 主从
- **关联偏离点**：progress/14 偏离点未登记此项
- **建议处置**：触发时修；同时在 `08_护栏清单.md` 不锁定（这不是 deliberate 决策，纯 MVP 留尾）

---

### TD-INFRA-04 · `init-slave.sh` 每次容器重建都 mysqldump，且无幂等保护

> **状态：✅ 已修复（Step 18 pre-cleanup C3）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：4
- **代码锚点**：`deploy/mysql/init-slave.sh`、`docker-compose.yml:77-97` slave-init 服务
- **现状**：slave-init 是 `restart: "no"` 的一次性容器，但若 `docker compose down && docker compose up` 时 volume 还在，slave 已经在 replicating，init 容器又会再跑一遍 `CHANGE REPLICATION SOURCE TO + mysqldump`。靠 Docker "只在 exit 0 后跳过" 兜底。
- **理想**：`init-slave.sh` 开头先 `SHOW REPLICA STATUS\G` 判定 IO_Running=Yes && SQL_Running=Yes，已在运行则 echo "already replicating, skip" 直接 exit 0；不依赖 Docker 行为。
- **触发条件**：开发者 `docker compose restart mysql-slave-init` / 重启脚本误触发 → mysqldump 中间 slave 进入异常状态。
- **重构成本**：S
- **关联 ADR**：PRD §5.2
- **关联偏离点**：progress/14 偏离点（init 容器幂等性未深究）
- **建议处置**：Step 18 修，捎带把 prod compose 一起做

---

### TD-INFRA-05 · Nginx 开源版仅 passive health check（架构所限）

- **档位**：2
- **代码锚点**：`deploy/nginx/nginx.conf:38-43`
- **现状**：注释明确承认"Active health check 要 nginx-plus 或第三方模块；当前只能 passive (`proxy_next_upstream error timeout http_503`)"。即 `/readiness` 返回 503 时，**该 upstream 不会被踢出**，要靠下一次实际请求碰到 503 才会切到对端，平均切流延迟 = 1 个失败请求。
- **理想**：换 Traefik / Caddy / nginx-plus，或加 `ngx_http_upstream_check_module` 主动 30s 探 `/health/ready`。或者上 k8s 用 readinessProbe 让 service 层处理。
- **触发条件**：单 app 实例假死（端口在但 readiness 报错），用户连续命中该实例的失败率拉高。
- **重构成本**：L（换网关 / 上 k8s）
- **关联 ADR**：PRD §3.3 / §10
- **关联偏离点**：progress/15 偏离点已登记
- **建议处置**：永久接受（写进 `08_护栏清单.md` 不许误删 passive 配置），等迁 k8s 时一并解决

---

### TD-INFRA-06 · Nginx 入口零防护：无速率限制、无 connection cap、无超大 body 限制

> **状态：✅ 已修复（Step 18 pre-cleanup B2）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：4
- **代码锚点**：`deploy/nginx/nginx.conf`（全文 60 行，无 `limit_req_zone` / `limit_conn_zone` / `client_max_body_size`）
- **现状**：公网入口只做了反代 + keepalive + passive failover。`/api/v1/user/send_code`、`/api/v1/posts`、`/api/v1/uploads/sign` 全部走应用层限流，但应用层挂掉时 Nginx 没有保护层；同时 `client_max_body_size` 默认 1m，一旦 OSS callback 链路改走 server proxy 立刻 413。
- **理想**：`limit_req_zone $binary_remote_addr zone=api:10m rate=20r/s` + `limit_conn perip 30` + `client_max_body_size 1m` 显式配。
- **触发条件**：MAU > 5w 或被刷接口；或上线 OSS 服务端代理上传。
- **重构成本**：S
- **关联 ADR**：PRD §11（限流）/ §10（接入层）
- **关联偏离点**：progress/15 偏离点未登记
- **建议处置**：Step 18 修

---

### TD-INFRA-07 · Docker Compose 缺资源限制（`mem_limit` / `cpus`）

- **档位**：4
- **代码锚点**：`docker-compose.yml` 全文（app1/app2/mysql/rabbitmq/prometheus/grafana 无 `deploy.resources.limits` 或 v2 的 `mem_limit/cpus`）
- **现状**：dev 容器可无限吃宿主机内存。一次 Go 调试 panic 内存爆炸把 MySQL 数据盘 oom-killer 干掉过的案例，dev 也会复现。
- **理想**：至少 mysql `mem_limit: 1g`、rabbitmq `mem_limit: 512m`、app `mem_limit: 512m`。prod compose 必须给死。
- **触发条件**：dev 单机调试时；prod 多容器同宿主时。
- **重构成本**：S
- **关联 ADR**：无对应 ADR
- **关联偏离点**：无
- **建议处置**：Step 18 修

---

### TD-INFRA-08 · `health.go` 503 响应绕过 `response` 包，且 `503001` 越段 / 不在 errcode 表

> **状态：✅ 已修复（Step 18 pre-cleanup A2）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：1
- **代码锚点**：
  - `internal/handler/health.go:79-84` 手写 `c.JSON(503, gin.H{"code": 503001, ...})`
  - `pkg/errcode/errcode.go` 未定义 `503001`
- **现状**：Readiness 失败时不走 `response.FromError`，而是直接 `c.JSON`；`code: 503001` 这个段位也不在 PRD §7 错误码分配表（错误码段位顶到 480xxx，503xxx 完全是裸数字）。
- **理想**：在 `errcode.go` 末追加 `ErrServiceUnavailable = New(503000, "service unavailable", 503)`，handler 改 `response.FromError(c, errcode.ErrServiceUnavailable.WithData(checks))`；或 `response` 包加一个 `response.Unavailable(c, data)` 快捷方法，统一 envelope。
- **触发条件**：监控告警分析 / 错误码看板对账；前端按 `code` 路由展示时遇到陌生编号。
- **重构成本**：S
- **关联 ADR**：PRD §7 错误码段位规范 + §九 评分卡 A5 / B1
- **关联偏离点**：无登记
- **建议处置**：立即修（A5 + B1 双重违规）

---

### TD-INFRA-09 · `config.prod.yaml` 缺 `consumer` / `cache` / `metrics` 三段（Config-First 一致性破洞）

> **状态：✅ 已修复（Step 18 pre-cleanup A1）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：1
- **代码锚点**：
  - `configs/config.prod.yaml`（共 109 行）只到 `ratelimit:` 段；无 consumer / cache / metrics
  - `configs/config.yaml` / `configs/config.docker.yaml` 三段齐全
- **现状**：dev / docker 两套都明确写了 ConsumerConfig（batch_size / flush_interval）、CacheConfig（like_state_ttl / feed_cache_ttl / pv_dedup_ttl）、MetricsConfig（namespace），prod 直接缺。一旦 prod 启动，三段全部走 Go 零值：batch_size=0 / flush_interval=0 → consumer flushLoop 死循环空转；cache TTL=0 → key 永不过期。
- **理想**：prod.yaml 三段必须给齐（参数可与 dev 不同）；并加一条 `pkg/config/config.go` Load 后 validate（零值 ⇒ fatal）兜底。
- **触发条件**：prod 第一次启动即触发（如果 Step 18 不补，必出 P0）。
- **重构成本**：S
- **关联 ADR**：CLAUDE.md §七 Config-First / §九 评分卡 B4 / conventions §1.1
- **关联偏离点**：无登记
- **建议处置**：立即修；同时把 `cfg.Validate()` 兜底加上

---

### TD-METRICS-01 · `pkg/metrics` 全局 nil 变量 + Init 调用顺序耦合

- **档位**：4
- **代码锚点**：
  - `pkg/metrics/metrics.go:21-43`（`HTTPRequestsTotal` 等 5 个全局 `*prometheus.CounterVec` / `*HistogramVec` / `*GaugeVec`）
  - `pkg/metrics/redis_hook.go:31` `if RedisCommandDuration == nil { return err }`
  - `internal/middleware/metrics.go:30` `if metrics.HTTPRequestsTotal == nil { return }`
- **现状**：所有指标是包级可变 `var`，Init 之前为 nil；middleware / hook 用 `if … == nil { return }` 容错。一旦 main 装配顺序错乱（如未来加新模块在 metrics.Init 之前注册 middleware），指标会"看似工作但没数据"，dashboard 静默掉零。
- **理想**：`metrics.Registry struct{ HTTPRequestsTotal *…; … }` + `New(namespace) *Registry`；middleware / hook 通过依赖注入拿到 registry；包级全局清零。
- **触发条件**：装配顺序变动；或测试代码忘了 `metrics.Init` 又调用 middleware（曾发生过，故有 nil 守卫）。
- **重构成本**：M（要改 middleware 签名、main.go 装配）
- **关联 ADR**：PRD §10 监控
- **关联偏离点**：progress/16 偏离点（已默认接受全局变量方案）
- **建议处置**：触发时修；Step 18 起 prod 后若 dashboard 出过一次"静默掉零"，立即重构

---

### TD-METRICS-02 · `/metrics` 公开在应用 8080 端口，无 token / IP 白名单 / 独立端口

> **状态：✅ 已修复（Step 18 pre-cleanup B3）→ Nginx 公网入口 `location = /metrics { return 404; }` 屏蔽；Prometheus 仍走内网 scrape app1/app2。详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：2
- **代码锚点**：
  - `cmd/server/main.go` 把 `/metrics` 挂在主 router 上（推断；当前未读但 verify_step16 默认走 8080）
  - `deploy/nginx/nginx.conf` 注释说"Nginx 故意不代理 /metrics"
  - `deploy/prometheus/prometheus.yml:13-14` 直接抓 `app1:8080` / `app2:8080`
- **现状**：靠 Docker 网络隔离当鉴权——Nginx 不代理 /metrics，但 app 容器自身的 8080 也没区分 public / admin 端口。任何攻入 Docker 内网的实体都可枚举指标（Go 版本 / Redis 抖动模式 / DB 连接池大小 → 用于指纹和侧信道）。
- **理想**：`/metrics` 挂在独立端口（如 9100）；Gin 起两个 Server，admin 端口只 listen 内网；或保持 8080 但加 metrics token 中间件 + Prometheus basic auth。
- **触发条件**：进入 prod；或安全合规审计；或与第三方共享集群网络。
- **重构成本**：S（独立端口）/ M（token + Prometheus 端配）
- **关联 ADR**：PRD §10 监控；CLAUDE.md §九（防御深度）
- **关联偏离点**：progress/16 偏离点（已默认接受内网隔离）
- **建议处置**：Step 18 修（独立端口最省力）

---

### TD-METRICS-03 · Grafana 默认凭据 `admin/admin` 写死在 compose

> **状态：✅ 已修复（Step 18 pre-cleanup B1）→ prod compose 用 `${APP_GRAFANA_ADMIN_PASSWORD:?Required}` 强制走 `.env.prod`，dev 仍保留 admin/admin（已 GUARD 接受）。详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：4
- **代码锚点**：`docker-compose.yml:252` `GF_SECURITY_ADMIN_PASSWORD: admin`
- **现状**：dev 用 admin/admin 可接受，但同一份 compose 注释里写 "dev only; override via env in prod"——也就是当前没有 prod compose（见 TD-INFRA-01），override 通道根本不存在。
- **理想**：dev `.env.dev` 注入；prod 通过 secrets。同时 Grafana 启用 `GF_AUTH_ANONYMOUS_ENABLED=false` 显式锁死。
- **触发条件**：上 prod；或 dev 机器误暴露 3000 端口到公网（黑客枚举 Grafana 几乎是入门 honeypot）。
- **重构成本**：S
- **关联 ADR**：无
- **关联偏离点**：无
- **建议处置**：Step 18 修

---

### TD-METRICS-04 · Prometheus 无 AlertManager / 无告警规则 / 7 天保留

- **档位**：4
- **代码锚点**：
  - `deploy/prometheus/prometheus.yml`（无 `rule_files`、无 `alerting` 段）
  - `docker-compose.yml:232` `--storage.tsdb.retention.time=7d`
  - 仓库无 `deploy/alertmanager/`
- **现状**：纯采集，无 SLO 报警；7 天保留对趋势分析够，但对历史复盘不够。
- **理想**：补 `deploy/prometheus/rules/*.yml`（HTTP 5xx 阈值 / Consumer 队列堆积 / DB 连接池打满 / Redis 命令错误率）+ AlertManager → Slack/钉钉；retention 升 30d 或外接 remote_write（Thanos / VictoriaMetrics）。
- **触发条件**：Step 18 上线，第一次 prod 事故时；面试官追问"你怎么知道服务挂了"。
- **重构成本**：L（含规则设计 + 告警通道接入）
- **关联 ADR**：PRD §10 监控；CLAUDE.md §一（面试材料）
- **关联偏离点**：progress/16 偏离点已登记"无 AlertManager"
- **建议处置**：Step 18 后立项

---

### TD-CI-01 · `pr.yml` 用 `staticcheck@latest`，无版本锁

> **状态：✅ 已修复（Step 18 pre-cleanup C1）→ 详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：4
- **代码锚点**：`.github/workflows/pr.yml:47` `go install honnef.co/go/tools/cmd/staticcheck@latest`
- **现状**：每次 CI 拉最新 staticcheck，未来某次 PR 会因为上游新增检查项突然变红，原作者却没动代码——侦错时浪费 30 分钟。
- **理想**：锁版本 `staticcheck@v0.5.1`（或当前实际使用版本）；同理 build/release 里的 `actions/setup-go@v5`（已锁主版本）应继续保持。
- **触发条件**：staticcheck 发新版引入新检查时；常态发生概率高（每 1–3 个月）。
- **重构成本**：S
- **关联 ADR**：CLAUDE.md §十二（学习硬性规定，可复现）
- **关联偏离点**：progress/17 偏离点未登记
- **建议处置**：立即修（5 分钟）

---

### TD-CI-02 · CI 未接 `verify_step*.sh`，无覆盖率门禁、无 golangci-lint

> **状态：✅ 部分修复（Step 18 pre-cleanup C2）→ verify-step17 已接入 pr.yml；覆盖率门禁 + golangci-lint 仍未做，留待 Step 18 之后立项。详见 [10_status-step18-pre-cleanup.md](10_status-step18-pre-cleanup.md)**

- **档位**：4
- **代码锚点**：`.github/workflows/pr.yml`（仅 `go vet` + `staticcheck` + `go test -race`）
- **现状**：仓库手写了 13 个 `scripts/verify_stepN.sh`（PRD/conventions 多次提它们是"每步收口必跑"），但 CI 不会执行其中任何一个。也没有覆盖率阈值，也没有 golangci-lint（staticcheck 是其子集）。
- **理想**：CI 加 `make verify-current-step`（按分支名取最近步骤）；接 `actions/codecov` 或 `go test -coverprofile=coverage.out` + threshold；上 golangci-lint（含 errcheck / govet / ineffassign / gocritic）。
- **触发条件**：开发者本地懒得跑 verify 直接 push，CI 不发现，合并到 main 后体检阶段才暴露——已经在 Step 12 体检前发生过一次（H1 RabbitMQ 实现缺失）。
- **重构成本**：M（golangci-lint 配规则需若干轮调优）
- **关联 ADR**：CLAUDE.md §七 / conventions §1
- **关联偏离点**：progress/17 偏离点未登记
- **建议处置**：Step 18 启动前补；最低限度先把 `make verify-step17` 接上 CI

---

### TD-CI-03 · `release.yml` 无 SBOM、无漏洞扫描、无 build-args 注入版本号

- **档位**：4
- **代码锚点**：`.github/workflows/release.yml:34-40`
- **现状**：tag → docker build → push GHCR。没有 `syft` 生成 SBOM、没有 `trivy` 镜像扫描、`Dockerfile` 也没 `ARG VERSION` → 镜像里跑 `cooking-platform --version` 看不到 git sha / tag。
- **理想**：build-push-action 前先 `actions/checkout@v4` → `syft scan` → `trivy image --severity HIGH,CRITICAL --exit-code 1`；Dockerfile 加 `ARG VERSION` + `LABEL org.opencontainers.image.version=$VERSION`。
- **触发条件**：第一次安全审计 / 客户合规问询；或镜像里某个间接依赖被披露 CVE。
- **重构成本**：S（每条独立可接）
- **关联 ADR**：PRD §12
- **关联偏离点**：progress/17 偏离点未登记
- **建议处置**：Step 18 修

---

## 本批批注（收口）

- **档位分布**：档 1 = 2 / 档 2 = 3 / 档 3 = 0 / 档 4 = 11 = 16 条
- **关键发现**：
  1. **"档 1 集中在 Step 15 收尾的两个细节"**：health.go 的 503001 越段 + config.prod.yaml 三段缺失。前者是 handler 模板违规（A5），后者是 Config-First 一致性破洞（B4），都属于"加一行就修"的便宜债，但放着不修就是 prod 首发翻车的引信。
  2. **Step 13–17 真正的债集中在"deploy 工件不完整"，不在代码里**：Batch 1 已把 RabbitMQ 客户端层（重连/DLX/ACK）审到位，Batch 6 这一波 16 条里有 11 条档 4，全部归 MVP 妥协——`docker-compose.prod.yml` 整个不存在、口令硬编码、Nginx 无防护、CI 不接 verify、release 不扫漏洞。这意味着 Step 18 的工作量远超"接 Sentinel / 启 prod compose"。
  3. **可观测性面"看似齐全实则单层"**：Prometheus + Grafana 接上了，但 `/metrics` 端点零鉴权、无 AlertManager、无告警规则、Grafana admin/admin——任何一个真正进入 prod 的人都会立刻察觉这是"演示就够了"的层次。建议把"独立 admin 端口"和"基本告警规则 4 条"列为 Step 18 的入门门槛。
- **新增护栏**：
  - **GUARD-19 候选**：Nginx 必须保留 `proxy_next_upstream error timeout http_503` 三件套（这是 passive failover 的唯一兜底，删掉就等于双实例变单实例 + 单点故障）——重构换网关之前不许动。
  - **GUARD-20 候选**：`mysql-slave-init` 的 `restart: "no"` 不许改成 `unless-stopped`（否则每次重启都会跑 mysqldump 把 slave 数据砸了）。
- **下一批衔接点**：
  - **Final 必须串起的跨批主线**：(a) Config-First 违反已在 Batch 2/3/4/5/6 累计 7+ 条（TD-USER / TD-SEARCH-01 / TD-LIKE / TD-FOLLOW-01 / TD-UPLOAD-02 / TD-AUDIT-01 / TD-CORS-01 / TD-INFRA-09），打包成"Config-First 补丁 PR"；(b) `requestIDKey` / `keyVerify` 一类的字符串散落问题（Batch 5 已开 TD-LOG-01）也要在 Final 汇总；(c) `503001` 与 `errcode.go` X=4/X=5 规则的冲突（Batch 5 TD-ERRCODE-01 已起头）在 Final 收编为"错误码段位 + HTTP status 双重一致性"统一处置。
  - **Final 跨模块系统性条目候选**：
    - TD-SYS-CONFIG-PARITY · prod.yaml 与 dev.yaml 字段对齐缺规约（CI 应 diff 两者）
    - TD-SYS-OBS-AUTHN · `/metrics` + Grafana + RabbitMQ Management 三个 admin 面零鉴权
    - TD-SYS-DEPLOY-PROD · 整个 prod 部署链条（compose / secrets / SBOM / 告警）缺失
    - TD-SYS-VERIFY-CI · 13 个 verify_step*.sh 未接 CI

---

_Batch 6 完成。等待用户「继续」进入 Final。_
