# cooking-platform

> 面向 22–28 岁独居 / 合租年轻人的烹饪内容分享平台后端 —— 按"出租屋 / 一个人的饭 / 露营野炊 / 家庭厨房 / 快手日常 / 打包便当 / 减脂餐 / 节气节日"八大场景组织内容，而不是传统菜系分类。

[![Go](https://img.shields.io/badge/Go-1.23-00ADD8?logo=go)](https://go.dev/)
[![Gin](https://img.shields.io/badge/Gin-1.10-00BCD4)](https://gin-gonic.com/)
[![MySQL](https://img.shields.io/badge/MySQL-8.0-4479A1?logo=mysql&logoColor=white)](https://www.mysql.com/)
[![Redis](https://img.shields.io/badge/Redis-7.2-DC382D?logo=redis&logoColor=white)](https://redis.io/)
[![RabbitMQ](https://img.shields.io/badge/RabbitMQ-3.x-FF6600?logo=rabbitmq&logoColor=white)](https://www.rabbitmq.com/)
[![Docker](https://img.shields.io/badge/Docker_Compose-prod-2496ED?logo=docker&logoColor=white)](https://docs.docker.com/compose/)
[![HTTPS](https://img.shields.io/badge/SSL_Labs-A%2B-brightgreen)](https://www.ssllabs.com/)
[![Status](https://img.shields.io/badge/status-live-brightgreen)](https://mellowck.com)

**生产地址**：https://mellowck.com · **阶段**：✅ 已上线（20/20 步收官，进入维护期）

---

## 生产实测 KPI

| 指标 | 数值 | 备注 |
| --- | --- | --- |
| 端到端 P99 延迟 | **≤ 40 ms** | 公网压测 5 个核心接口，wrk2 恒定速率 |
| 读流量从库占比 | **91 %** | GORM + DBResolver Random Policy 实证 |
| SSL Labs 评级 | **A+** | Mozilla Intermediate + HSTS 1 年 |
| 容器健康度 | **12 / 12** | 双 Go 实例 + Nginx + MySQL 主从 + Redis Sentinel + RabbitMQ + Prometheus / Grafana |
| 运行成本 | **2C4G** | 阿里云香港 ECS 单机承载完整生产栈 |

---

## 技术栈

| 层 | 选型 |
| --- | --- |
| 语言 / 框架 | Go 1.23 · Gin · go-playground/validator |
| 配置 / 日志 | Viper（dev / docker / prod 三 YAML 校验一致）· Zap + Lumberjack（PII 脱敏） |
| 数据库 | MySQL 8.0（GTID 主从）· GORM · DBResolver（Random Policy 读写分离） |
| 缓存 | Redis 7.2 Sentinel · 旁路缓存 + WriteMarker（读后写一致性） |
| 消息队列 | RabbitMQ（持久化 + ACK + DLX）· `EventBus` 接口抽象（dev Channel / prod RabbitMQ） |
| 对象存储 | 阿里云 OSS（PresignedURL 客户端直传 + nonce 防重放） |
| 鉴权 | JWT (golang-jwt/v5) + Redis 黑名单 · 短信 OTP |
| 安全 | AES-GCM 加密手机号 + SHA-256 索引 · 内容安全审核（阿里云 Green API） |
| 可观测 | Prometheus 客户端埋点（HTTP 延迟 / MySQL 池 / Redis 命中率 / Consumer backlog）· Grafana 看板 · Alertmanager |
| 部署 | Docker Compose（dev / prod 双栈）· Nginx 反代 + 限流 · Let's Encrypt（webroot 自动续期） |
| CI | GitHub Actions（PR 检查 + main 构建 + staticcheck + 集成测试） |
| 迁移 | golang-migrate |

---

## 架构原则

### 单体优先，未来可拆
`cmd/server/main.go` 是唯一入口；模块之间用 `EventBus` 解耦而不是直接调用，为后续拆分留口子。**不引入**服务发现、分布式追踪、分布式事务。

### 三层 + 旁路缓存 + 异步事件
```
internal/handler/    bind → validate → service → response
internal/service/    业务逻辑（不直接操作 DB / Redis）
internal/repository/ DB 操作 + ErrXxxNotFound
internal/cache/      Redis 旁路（含 WriteMarker / EventDedupCache）
internal/event/      EventBus 接口 + ChannelBus / RabbitMQBus
internal/consumer/   LikeConsumer / PVConsumer / AuditConsumer / CountConsumer + DLXMonitor
```

### 接口先行 · Mock + Real 三件套
每个外部依赖按相同模式：`pkg/<dep>/<dep>.go`（interface）+ `mock.go`（dev / 测试）+ `aliyun.go`（生产）。通过 `<dep>.provider: mock | aliyun` 切换，零业务代码改动。已落地：`pkg/sms` · `pkg/audit` · `pkg/oss`。

### 错误码段位预分配
6 位三段：`XYYZZZ` = HTTP 段 + 模块段 + 序号。例如 `410001` 是用户模块第 1 个错误，`420001` 是内容模块第 1 个。错误码是 API 契约 ABI 的一部分，**已发布的不可删改**。

---

## 功能模块

26 个公开 HTTP 接口，7 个业务模块：

| 模块 | 前缀 | 关键能力 |
| --- | --- | --- |
| **Auth** | `/api/v1/auth` | 短信 OTP 登录 · JWT + Refresh Token · 登出黑名单 |
| **User** | `/api/v1/users` | 资料查询 / 修改 · 用户作品列表 · 关注 / 取关 / 关注者 / 关注列表 |
| **Post** | `/api/v1/posts` | 多步骤菜谱发布 · 详情 · 作者自删 · 点赞 / 取消点赞 / 点赞状态 |
| **Feed** | `/api/v1/feed` | Cursor 分页 · Redis 版本号缓存 · 场景标签过滤 |
| **Search** | `/api/v1/search` | MySQL FULLTEXT (ngram_token_size=2) · per-user 限流 |
| **Upload** | `/api/v1/upload` | OSS PresignedURL 直传 · nonce 防重放 · 回调审核入队 |
| **Health** | `/health` `/readiness` `/metrics` | 存活 / 就绪 / Prometheus 指标 |

**永久排除**：评论（产品决策）· 私信 / 直播 / 付费内容（超出 MVP）。视频是延期不是排除。

---

## 项目结构

```
cooking-platform/
├── cmd/
│   ├── server/                # 主入口
│   └── migrate-phone/         # 一次性 AES-GCM 手机号加密迁移工具
├── internal/
│   ├── handler/   service/   repository/   cache/
│   ├── consumer/  event/     middleware/   model/
├── pkg/
│   ├── audit/  config/  crypto/  errcode/  graceful/
│   ├── jwt/    logger/  metrics/ oss/      response/
│   ├── sms/    validator/
├── deploy/
│   ├── nginx/  mysql/  redis/  rabbitmq/
│   ├── prometheus/  grafana/  alertmanager/  blackbox/  certbot/
├── configs/                   # config.yaml · config.docker.yaml · config.prod.yaml
├── migrations/                # golang-migrate up/down 文件对
├── scripts/                   # verify_step{7..17,20}.sh · stress_test.sh · check_config_parity.sh
├── docs/
│   ├── prd/                   # 权威 PRD v3.0
│   ├── project/               # PROJECT_RETROSPECTIVE.md（12 个关键技术决策复盘）
│   ├── api-spec/              # 接口契约
│   ├── storylines/            # 工程师视角的踩坑与决策叙事（步骤 1-20）
│   ├── progress/              # 每步交付追踪
│   ├── drills/                # 故障演练 Playbook
│   └── ...                    # changes/debt/conventions/audit/ops/maintenance/stress 等 22 个子目录
├── docker-compose.yml         # dev 栈（MySQL + Redis）
├── docker-compose.prod.yml    # prod 栈（12 容器）
├── Makefile                   # 构建 / 测试 / 迁移 / 验证
└── CLAUDE.md                  # 项目长期维护规范
```

---

## 本地开发

需要 Go 1.23+、Docker、`make`、`golang-migrate`。

```bash
# 1. 拉起 dev 基础设施（MySQL + Redis）
make docker-up

# 2. 应用数据库迁移
make migrate-up

# 3. 运行服务（CONFIG_PATH 默认 configs/config.yaml）
make run

# 4. 验证某个模块（每个 step 都有端到端脚本）
make verify-step7    # 搜索
make verify-step8    # 关注
make verify-step9    # 上传
# ...
```

服务起在 `:8080`。SMS / OSS / 审核默认走 mock，不需要任何阿里云密钥。

---

## 测试与 CI

```bash
make test             # go test ./... -race -count=1
make test-cover       # HTML 覆盖率报告
make lint             # go vet + staticcheck
make check-config-parity  # 三 YAML 顶级 key 集合校验
```

**GitHub Actions**：
- `pr.yml`：PR 触发，起 MySQL 8.0 + Redis 7.2 service，跑 staticcheck + 单测 + 集成测试 + `verify-step17` 自检
- `build.yml`：main push 触发，go build + Docker 镜像 smoke test

**端到端验证脚本**（`scripts/verify_step*.sh`）：每个里程碑一个，对应业务流程从 SMS 抓码 → 登录 → 调用主流程 → 校验数据落库 / 缓存命中 / 事件消费。

**压测**：`make stress-test` 用 wrk2 恒定速率打 5 个核心接口，输出 P50/P95/P99 + 期间主从读流量比例实证。

---

## 项目里程碑（20 步 · 4 轮）

完整故事在 [`docs/storylines/`](./docs/storylines/) —— 每一步都配 1 篇工程师视角的踩坑叙事 + 1 个可重放的 verify 脚本。

| 轮次 | 范围 | 主题 |
| --- | --- | --- |
| 第 1 轮 | Step 1-6 | 骨架打地基：配置 / 日志 / 错误码 / 数据层 / 用户 / 内容 |
| 第 2 轮 | Step 7-10 | 业务铺面：搜索 / 关注 / OSS 上传 / 内容审核 |
| 第 3 轮 | Step 11-17 | 工程化提质：JWT 黑名单 / Redis Sentinel / RabbitMQ + DLX / 监控 / 限流 / 安全头 / CI |
| 第 4 轮 | Step 18-20 | 上生产：服务器 + HTTPS / 数据库迁移 + 部署联调 / 公网验证 + 压测 |

**重点复盘**：[`docs/project/PROJECT_RETROSPECTIVE.md`](./docs/project/PROJECT_RETROSPECTIVE.md) 收录了 12 个关键技术决策的完整背景（为什么不用 ES 而用 MySQL FULLTEXT、为什么 Redis 用 Sentinel 而不是 Cluster、为什么手机号要 AES-GCM + SHA-256 双轨等等）。

---

## 文档导航

| 优先级 | 文档 | 用途 |
| --- | --- | --- |
| P0 | [`docs/prd/PRD-Final-v3.0.md`](./docs/prd/PRD-Final-v3.0.md) | 接口契约 / 错误码 / 架构决策唯一真相源 |
| P0 | [`docs/project/PROJECT_RETROSPECTIVE.md`](./docs/project/PROJECT_RETROSPECTIVE.md) | 项目总复盘 |
| P1 | [`docs/api-spec/`](./docs/api-spec/) | 13 份接口规范（用户 / 内容 / 互动 / 关注 / 搜索 / 上传 / 审核） |
| P1 | [`docs/progress/18-20`](./docs/progress/) | 服务器 + HTTPS + 部署联调 + 公网验证全过程 |
| P2 | [`docs/storylines/`](./docs/storylines/) | 工程师视角的步骤叙事 |
| P2 | [`docs/drills/`](./docs/drills/) | 故障演练 Playbook |
| P2 | [`CLAUDE.md`](./CLAUDE.md) | 长期维护规范（运维 SOP / 红线 / 遗留债务清单） |

---

## 路线图（v2 候选）

进入 [`CLAUDE.md §11 遗留债务清单`](./CLAUDE.md) 任意一条即视为开启 cooking-platform v2：

1. prod 日志归集（Loki / Promtail）
2. nginx.conf 由单文件挂载改目录挂载（消除 inode 失效坑）
3. 多机客户端压测方案（k6 cloud / 自建 wrk 集群）
4. HSTS preload 名单注册
5. 关注 Feed 流（推 / 拉 / 混合选型）
6. 视频内容（转码 + 存储成本评估）

---

## 致谢与背景

本仓库是一份**从 0 到上线的完整工程实践记录**：20 步、4 轮、覆盖从骨架搭建到生产实证的所有环节。每一个偏离 PRD 的实现都登记在对应步骤的"偏离点"章节，作为后续版本演进的依据。

人机协作模式：核心代码作者 [@quqxiaoli](https://github.com/quqxiaoli)，AI 协作者 Claude（Anthropic）。

---

> *"单体优先，未来可拆；接口先行，依赖可换；偏离登记，演进可溯。"*
