# 烹饪内容分享平台 · 技术架构设计文档 v2.1

> **文档状态**：草稿（待确认）
> **所属阶段**：阶段 3 — 技术架构设计
> **作者**：首席 AI 工程师
> **创建日期**：2026-04-15
> **依赖文档**：PRD-Phase1-立项文档-v1.2.md · PRD-Phase2-需求文档.md
> **技术栈**：Go · MySQL · Redis · RabbitMQ · Docker · Nginx
> **变更记录**：
> - v1.0 — 初版架构（单实例、无高可用、offset 分页）
> - v2.0 — 全面升级至大厂生产标准：高可用部署、游标分页、OSS 直传、审核状态机、可观测性体系、安全合规、CI/CD、降级预案等
> - v2.1 — 修复 7 项自查问题 + 引入消息队列（RabbitMQ）替代 Redis List/Set 驱动的异步链路 + EventBus 接口抽象（支持 MVP 阶段 Go Channel 实现与生产 MQ 实现无缝切换）

---

## 目录

1. [[#1. 整体架构描述|整体架构描述]]
2. [[#2. Go 项目目录结构|Go 项目目录结构]]
3. [[#3. 各层职责与设计原则|各层职责与设计原则]]
4. [[#4. 消息队列设计（EventBus 抽象 + RabbitMQ）|消息队列设计（EventBus 抽象 + RabbitMQ）]]
5. [[#5. 数据库设计（MySQL）|数据库设计（MySQL）]]
6. [[#6. Redis 缓存策略|Redis 缓存策略]]
7. [[#7. RESTful API 设计规范|RESTful API 设计规范]]
8. [[#8. 图片上传方案（OSS 直传）|图片上传方案（OSS 直传）]]
9. [[#9. 内容审核流程|内容审核流程]]
10. [[#10. 安全合规设计|安全合规设计]]
11. [[#11. 可观测性体系|可观测性体系]]
12. [[#12. 降级预案与容灾设计|降级预案与容灾设计]]
13. [[#13. Docker 高可用部署方案|Docker 高可用部署方案]]
14. [[#14. CI/CD 流水线|CI/CD 流水线]]
15. [[#15. 数据库迁移管理|数据库迁移管理]]
16. [[#16. 关键技术决策记录（ADR）|关键技术决策记录（ADR）]]

---

## v2.0 → v2.1 变更摘要

### 问题修复（7 项）

| # | 问题 | 修复方案 |
|---|---|---|
| 1 | Sentinel 配置挂载 `:ro` 导致启动失败 | 改为 entrypoint 脚本动态生成配置，三个 Sentinel 各自独立数据卷 |
| 2 | sentinel.conf 中 `${REDIS_PASSWORD}` 不会被替换 | entrypoint 脚本中用 `sed` 替换变量后再启动 |
| 3 | MySQL 从库缺少复制初始化步骤 | 新增 `init-slave.sh` 脚本，首次启动自动执行 `CHANGE REPLICATION SOURCE TO` |
| 4 | UserCountSyncWorker 依赖 `posts.updated_at` 追踪点赞变化用户，但点赞不更新此字段 | 改为 MQ 事件驱动：点赞事件消息中携带 `user_id`（帖子作者），Consumer 精准更新 |
| 5 | `idx_audit_status` 对 `IN (1,3)` 查询效率存疑 | posts 表新增 `is_visible TINYINT` 冗余字段（审核通过时置 1），Feed 查询改为 `WHERE is_visible=1`，单值索引 |
| 6 | migrations 目录未列全 down 文件 | 补全所有 down 文件 |
| 7 | prometheus.yml 和 grafana dashboard 被引用但无内容 | 补充完整配置 |

### 新增：消息队列（RabbitMQ）

| 变更 | 说明 |
|---|---|
| 新增 EventBus 接口抽象 | 定义 `EventPublisher` + `EventConsumer` 接口，业务代码面向接口编程 |
| MVP 实现：Go Channel | 进程内 Channel 实现，零外部依赖，快速跑通主流程 |
| 生产实现：RabbitMQ | 持久化队列 + ACK 确认 + 死信队列 + 重试机制，确保消息不丢失 |
| 替换 Redis List 审核回调 | 审核结果通过 MQ 投递，Consumer 更新 audit_status |
| 替换 Redis Set 脏标记同步 | 点赞/PV 事件通过 MQ 投递，Consumer 批量聚合后写 MySQL |
| docker-compose 新增 RabbitMQ | 含管理面板，支持持久化 |

---

## 1. 整体架构描述

### 1.1 架构总览（v2.1 高可用版）

```
                         ┌──────────────────┐
                         │    CDN（阿里云）   │
                         │  静态资源 + 图片   │
                         └────────┬─────────┘
                                  │
                         ┌────────▼─────────┐
                         │   云 SLB / DNS    │
                         │   域名 + HTTPS    │
                         └────────┬─────────┘
                                  │
              ┌───────────────────▼───────────────────┐
              │           Nginx（反向代理集群）          │
              │   SSL 终止 · 限速 · 健康检查 · 负载均衡  │
              └───┬───────────────────────────────┬───┘
                  │ upstream least_conn           │
        ┌─────────▼─────────┐           ┌─────────▼─────────┐
        │   Go App 实例 #1   │           │   Go App 实例 #2   │
        │  ┌──────────────┐  │           │  ┌──────────────┐  │
        │  │  Middleware   │  │           │  │  Middleware   │  │
        │  ├──────────────┤  │           │  ├──────────────┤  │
        │  │   Handler    │  │           │  │   Handler    │  │
        │  ├──────────────┤  │           │  ├──────────────┤  │
        │  │   Service    │──┼──publish──┼──│   Service    │  │
        │  ├──────────────┤  │           │  ├──────────────┤  │
        │  │  Repository  │  │           │  │  Repository  │  │
        │  └──────────────┘  │           │  └──────────────┘  │
        └─────┬───┬──┬──┬───┘           └─────┬───┬──┬──┬───┘
              │   │  │  │                     │   │  │  │
              │   │  │  └──────────┬──────────┘   │  │  │
              │   │  │             │               │  │  │
              │   │  │    ┌────────▼──────────┐    │  │  │
              │   │  │    │   RabbitMQ 集群    │    │  │  │
              │   │  │    │  ┌──────────────┐  │    │  │  │
              │   │  │    │  │ Exchange     │  │    │  │  │
              │   │  │    │  │  ├─ like.q   │  │    │  │  │
              │   │  │    │  │  ├─ pv.q     │  │    │  │  │
              │   │  │    │  │  ├─ audit.q  │  │    │  │  │
              │   │  │    │  │  └─ count.q  │  │    │  │  │
              │   │  │    │  └──────────────┘  │    │  │  │
              │   │  │    └────────┬──────────┘    │  │  │
              │   │  │             │               │  │  │
              │   │  │    ┌────────▼──────────┐    │  │  │
              │   │  │    │  MQ Consumer      │    │  │  │
              │   │  │    │  (独立进程或协程)   │    │  │  │
              │   │  │    │  ├─ LikeConsumer  │    │  │  │
              │   │  │    │  ├─ PVConsumer    │    │  │  │
              │   │  │    │  ├─ AuditConsumer │    │  │  │
              │   │  │    │  └─ CountConsumer │    │  │  │
              │   │  │    └───┬──────┬───────┘    │  │  │
              │   │  │        │      │            │  │  │
    ┌─────────▼───▼──┼────────▼──────┼────────────▼──▼──┘
    │                │               │
    │  ┌─────────────▼───────────────▼──┐
    │  │   Redis Sentinel 集群          │
    │  │  ┌────────┐ ┌────────┐ ┌────────┐
    │  │  │Master  │ │Slave-1 │ │Slave-2 │
    │  │  └────────┘ └────────┘ └────────┘
    │  │  Sentinel×3 自动故障转移
    │  └────────────────────────────────┘
    │
    │  ┌─────────────────────────────────┐
    │  │   MySQL 主从复制                 │
    │  │  ┌──────────┐    ┌──────────┐   │
    │  │  │ Master   │───▶│ Slave    │   │
    │  │  │ (读写)   │    │ (只读)   │   │
    │  │  └──────────┘    └──────────┘   │
    │  │  自动初始化复制关系（init-slave.sh）
    │  └─────────────────────────────────┘
    │
    │  ┌─────────────────────────────────┐
    └──│   阿里云 OSS（图片存储）          │
       │   客户端直传 · 服务端签名          │
       └─────────────────────────────────┘
       ┌─────────────────────────────────┐
       │   阿里云短信 SMS                 │
       └─────────────────────────────────┘
       ┌─────────────────────────────────┐
       │   阿里云内容安全（图片/文字审核）  │
       └─────────────────────────────────┘
```

### 1.2 关键设计决策总览

| 决策 | 选型 | 理由 |
|---|---|---|
| Web 框架 | `gin` | 社区最成熟、中间件生态完整、文档案例最丰富 |
| ORM | `GORM` | 软删除、链式查询、DBResolver 读写分离插件 |
| Redis 客户端 | `go-redis/v9` | 原生支持 Sentinel 模式、连接池完善 |
| 消息队列 | `RabbitMQ`（接口抽象，MVP 可用 Channel） | 持久化 + ACK + 死信队列，比 Redis List 可靠；比 Kafka 运维简单 |
| MQ 客户端 | `amqp091-go` | RabbitMQ 官方 Go 客户端 |
| 配置管理 | `viper` | YAML + 环境变量覆盖，适配 Docker 部署 |
| 日志 | `zap` + `lumberjack` | 结构化 JSON 输出 + 日志轮转 |
| JWT | `golang-jwt/jwt/v5` | 标准实现 |
| 指标采集 | `prometheus/client_golang` | 业界标准，与 Grafana 无缝集成 |
| 数据库迁移 | `golang-migrate` | 版本化迁移管理，支持 up/down/force |
| 图片上传 | 阿里云 OSS STS 临时凭证 + 客户端直传 | Go 服务器不过图片流量 |
| 短信 | 阿里云短信 SDK | 国内覆盖率高、到达率稳定 |
| 内容审核 | 阿里云内容安全 | 文字 + 图片双通道异步审核 |

### 1.3 流量模型估算（用于容量规划）

| 指标 | MVP 初期 | 中期目标 | 设计上限 |
|---|---|---|---|
| DAU | 1,000 | 100,000 | 1,000,000 |
| 日发帖量 | 50 | 5,000 | 50,000 |
| 日点赞量 | 500 | 100,000 | 2,000,000 |
| 日搜索量 | 200 | 50,000 | 500,000 |
| 日详情页 PV | 5,000 | 500,000 | 10,000,000 |
| posts 表总行数 | 1,000 | 500,000 | 20,000,000 |
| likes 表总行数 | 5,000 | 5,000,000 | 500,000,000 |
| MQ 日消息量 | 6,000 | 600,000 | 12,000,000 |
| 峰值 QPS（读） | 10 | 2,000 | 30,000 |
| 峰值 QPS（写） | 2 | 200 | 3,000 |

> **设计原则**：所有架构决策以"中期目标"（10 万 DAU）为基准设计，同时确保在"设计上限"（百万 DAU）到达前有清晰的扩容路径。

### 1.4 异步事件流全景

```
┌─────────────────────────────────────────────────────────────────┐
│                      事件驱动异步链路                             │
├─────────────┬───────────────┬──────────────────────────────────┤
│ 触发场景     │ 事件类型       │ 消费逻辑                         │
├─────────────┼───────────────┼──────────────────────────────────┤
│ 用户点赞     │ event.like    │ 1. INSERT likes 表               │
│             │               │ 2. UPDATE posts.like_count       │
│             │               │ 3. 发出 event.count（作者总赞数） │
├─────────────┼───────────────┼──────────────────────────────────┤
│ 用户取消点赞 │ event.unlike  │ 1. DELETE likes 表               │
│             │               │ 2. UPDATE posts.like_count       │
│             │               │ 3. 发出 event.count              │
├─────────────┼───────────────┼──────────────────────────────────┤
│ 访问详情页   │ event.pv      │ 1. INCR pv:cnt（Redis 计数）     │
│             │               │ 2. 批量 UPDATE posts.view_count  │
├─────────────┼───────────────┼──────────────────────────────────┤
│ 内容审核完成 │ event.audit   │ 1. UPDATE posts.audit_status     │
│             │               │ 2. UPDATE posts.is_visible       │
│             │               │ 3. 写 audit_log                  │
│             │               │ 4. INCR feed:ver（缓存失效）      │
├─────────────┼───────────────┼──────────────────────────────────┤
│ 点赞/关注   │ event.count   │ UPDATE users 冗余计数字段          │
│ 变化        │               │（post_count / total_likes /       │
│             │               │  follower_count / following_count）│
├─────────────┼───────────────┼──────────────────────────────────┤
│ 发布内容     │ event.post    │ 1. 提交审核 API                   │
│             │               │ 2. INCR feed:ver                  │
│             │               │ 3. 发出 event.count（发帖数）      │
└─────────────┴───────────────┴──────────────────────────────────┘

v2.0 方案（已废弃）         v2.1 方案（当前）
Redis List + 定时轮询       EventBus 接口 + MQ 消息驱动
Redis Set 脏标记 + 全扫描   事件携带完整上下文，精准更新
5~15 分钟延迟               秒级延迟（Consumer 实时消费）
Redis 宕机 = 数据丢失       MQ 持久化 + ACK = 消息不丢失
```

---

## 2. Go 项目目录结构

```
cooking-platform/
├── cmd/
│   └── server/
│       └── main.go                  # 程序入口：初始化依赖、注册路由、优雅启停
│
├── internal/
│   ├── handler/                     # Handler 层：HTTP 路由处理
│   │   ├── user.go
│   │   ├── post.go
│   │   ├── feed.go
│   │   ├── like.go
│   │   ├── search.go
│   │   ├── follow.go
│   │   ├── upload.go
│   │   └── health.go
│   │
│   ├── service/                     # Service 层：业务逻辑 + 事件发布
│   │   ├── user.go
│   │   ├── post.go
│   │   ├── feed.go
│   │   ├── like.go
│   │   ├── search.go
│   │   ├── follow.go
│   │   └── audit.go
│   │
│   ├── repository/                  # Repository 层：数据访问
│   │   ├── user.go
│   │   ├── post.go
│   │   ├── like.go
│   │   ├── follow.go
│   │   └── search.go
│   │
│   ├── model/
│   │   ├── user.go
│   │   ├── post.go
│   │   ├── like.go
│   │   ├── follow.go
│   │   ├── scene_tag.go             # 场景标签常量（TINYINT 映射）
│   │   └── dto/
│   │       ├── user_dto.go
│   │       ├── post_dto.go
│   │       └── pagination.go        # 游标分页通用结构
│   │
│   ├── middleware/
│   │   ├── auth.go
│   │   ├── ratelimit.go
│   │   ├── logger.go
│   │   ├── cors.go
│   │   ├── metrics.go
│   │   ├── recovery.go
│   │   └── request_id.go
│   │
│   ├── cache/                       # Redis 操作封装
│   │   ├── feed.go                  # Feed 缓存（版本号机制）
│   │   ├── like.go                  # 点赞状态判断（SISMEMBER）
│   │   ├── pv.go                    # 浏览量去重
│   │   ├── session.go               # 用户 Session / 封禁 / JWT 黑名单
│   │   ├── sms.go                   # 验证码存储 + 发送限制
│   │   └── bloom.go                 # Bloom Filter（热帖点赞去重降级）
│   │
│   ├── event/                       # 事件总线（核心抽象层）
│   │   ├── types.go                 # 事件类型定义
│   │   ├── bus.go                   # EventPublisher + EventConsumer 接口
│   │   ├── channel.go               # MVP 实现：Go Channel（进程内）
│   │   └── rabbitmq.go              # 生产实现：RabbitMQ
│   │
│   └── consumer/                    # 事件消费者（替代 v2.0 的 worker/）
│       ├── manager.go               # Consumer 生命周期管理（优雅停机）
│       ├── like_consumer.go         # 点赞事件 → likes 表 + posts.like_count
│       ├── pv_consumer.go           # PV 事件 → posts.view_count
│       ├── audit_consumer.go        # 审核结果 → posts.audit_status + is_visible
│       └── count_consumer.go        # 冗余计数 → users 表字段更新
│
├── pkg/
│   ├── config/
│   │   └── config.go
│   ├── response/
│   │   └── response.go
│   ├── errcode/
│   │   └── errcode.go
│   ├── logger/
│   │   └── logger.go
│   ├── validator/
│   │   └── validator.go
│   ├── sms/
│   │   └── aliyun.go
│   ├── oss/
│   │   ├── presign.go
│   │   └── callback.go
│   ├── audit/
│   │   └── aliyun.go
│   ├── crypto/
│   │   ├── aes.go
│   │   └── mask.go
│   ├── graceful/
│   │   └── shutdown.go
│   └── metrics/
│       └── metrics.go
│
├── configs/
│   ├── config.yaml
│   ├── config.prod.yaml
│   └── config.test.yaml
│
├── migrations/
│   ├── 000001_create_users.up.sql
│   ├── 000001_create_users.down.sql
│   ├── 000002_create_posts.up.sql
│   ├── 000002_create_posts.down.sql
│   ├── 000003_create_post_steps.up.sql
│   ├── 000003_create_post_steps.down.sql
│   ├── 000004_create_post_images.up.sql
│   ├── 000004_create_post_images.down.sql
│   ├── 000005_create_ingredients.up.sql
│   ├── 000005_create_ingredients.down.sql
│   ├── 000006_create_tags.up.sql
│   ├── 000006_create_tags.down.sql
│   ├── 000007_create_likes.up.sql
│   ├── 000007_create_likes.down.sql
│   ├── 000008_create_follows.up.sql
│   ├── 000008_create_follows.down.sql
│   └── 000009_create_audit_log.up.sql
│   └── 000009_create_audit_log.down.sql
│
├── scripts/
│   ├── backup.sh
│   ├── seed.sh
│   └── migrate.sh
│
├── deploy/
│   ├── Dockerfile
│   ├── docker-compose.yml           # 开发环境（单实例，Channel 模式）
│   ├── docker-compose.prod.yml      # 生产环境（高可用，RabbitMQ 模式）
│   ├── nginx/
│   │   ├── nginx.conf
│   │   └── upstream.conf
│   ├── redis/
│   │   └── sentinel-entrypoint.sh   # ★ 修复 #1/#2：动态生成配置
│   ├── mysql/
│   │   ├── master.cnf
│   │   ├── slave.cnf
│   │   └── init-slave.sh            # ★ 修复 #3：复制初始化脚本
│   ├── rabbitmq/
│   │   ├── rabbitmq.conf
│   │   └── definitions.json         # 预定义 Exchange/Queue/Binding
│   ├── prometheus/
│   │   └── prometheus.yml           # ★ 修复 #7：完整配置
│   └── grafana/
│       └── dashboards/
│           └── cooking-platform.json # ★ 修复 #7：完整看板
│
├── .github/
│   └── workflows/
│       ├── ci.yml
│       └── cd.yml
│
├── go.mod
├── go.sum
├── Makefile
├── .env.example
└── .gitignore
```

### 2.1 v2.0 → v2.1 目录结构变更

| 变更 | 说明 |
|---|---|
| `internal/worker/` → `internal/consumer/` | 从定时轮询 Worker 改为 MQ 事件消费者 |
| 新增 `internal/event/` | EventBus 接口 + Channel 实现 + RabbitMQ 实现 |
| 新增 `deploy/redis/sentinel-entrypoint.sh` | 替代 sentinel.conf 静态挂载 |
| 新增 `deploy/mysql/init-slave.sh` | MySQL 从库复制初始化 |
| 新增 `deploy/rabbitmq/` | RabbitMQ 配置 + 预定义拓扑 |
| 补全 `deploy/prometheus/prometheus.yml` | 完整 Prometheus 采集配置 |
| 补全 `deploy/grafana/dashboards/` | 完整 Grafana 看板 JSON |
| 补全所有 `migrations/*.down.sql` | 每个 up 都有对应 down |

---

## 3. 各层职责与设计原则

### 3.1 Handler 层

**职责**：路由注册、请求参数绑定与校验、调用 Service、格式化响应。

**原则**：
- Handler 不包含任何业务逻辑，只做"请求 → Service → 响应"的转换
- 所有参数校验使用 `binding:"required"` + 自定义 validator
- 通过 `middleware/request_id.go` 注入的 `X-Request-ID` 串联全链路日志
- 返回统一的 `response.JSON()` 格式

```go
type UserHandler struct {
    userService service.UserService
}

func (h *UserHandler) Register(c *gin.Context) {
    var req dto.RegisterRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        response.BadRequest(c, errcode.ErrInvalidParams, err.Error())
        return
    }
    resp, err := h.userService.Register(c.Request.Context(), &req)
    if err != nil {
        response.FromError(c, err)
        return
    }
    response.Success(c, resp)
}
```

### 3.2 Service 层

**职责**：核心业务逻辑、事务控制、跨 Repository 协调、Redis 操作编排、读写分离路由、**事件发布**。

**原则**：
- Service 方法接收 `context.Context`，所有操作传递 ctx
- 事务在 Service 层开启和提交/回滚
- Service 不直接引用 `*gin.Context`，保持可单元测试
- 写操作走主库，读操作走从库（通过 GORM DBResolver）
- **异步操作通过 EventPublisher 发布事件，不直接操作 MQ**
- 所有外部调用（OSS、SMS、审核 API）设置超时 + 重试

```go
type UserService interface {
    Register(ctx context.Context, req *dto.RegisterRequest) (*dto.RegisterResponse, error)
    Login(ctx context.Context, req *dto.LoginRequest) (*dto.LoginResponse, error)
    GetProfile(ctx context.Context, userID, viewerID int64) (*dto.UserProfileResponse, error)
    UpdateProfile(ctx context.Context, userID int64, req *dto.UpdateProfileRequest) error
}

// Service 持有 EventPublisher，不关心底层是 Channel 还是 RabbitMQ
type LikeService struct {
    masterDB  *gorm.DB
    slaveDB   *gorm.DB
    likeRepo  repository.LikeRepository
    cache     cache.LikeCache
    publisher event.EventPublisher  // ← 面向接口
}
```

### 3.3 Repository 层

**职责**：所有数据库读写操作的封装，屏蔽 GORM 细节。

**原则**：
- Repository 方法只做单表或简单关联查询
- 接受 `*gorm.DB` 参数，支持外部传入事务 db
- 软删除统一用 GORM 的 `DeletedAt` 字段
- 不在 Repository 层做缓存

```go
type PostRepository interface {
    Create(ctx context.Context, db *gorm.DB, post *model.Post) error
    GetByID(ctx context.Context, db *gorm.DB, id int64) (*model.Post, error)
    ListFeed(ctx context.Context, db *gorm.DB, sceneTag int8, cursor time.Time, limit int) ([]*model.Post, error)
    SoftDelete(ctx context.Context, db *gorm.DB, id int64) error
    UpdateVisibility(ctx context.Context, db *gorm.DB, postID int64, isVisible int8) error
    BatchUpdateLikeCounts(ctx context.Context, db *gorm.DB, updates map[int64]int) error
    BatchUpdateViewCounts(ctx context.Context, db *gorm.DB, updates map[int64]int) error
}
```

### 3.4 Consumer 层（替代 v2.0 的 Worker 层）

**职责**：消费 EventBus 中的事件消息，执行异步持久化和副作用操作。

**v2.0 → v2.1 核心变化**：

| 维度 | v2.0 Worker | v2.1 Consumer |
|---|---|---|
| 驱动方式 | 定时轮询（每 N 分钟扫 Redis） | 事件驱动（MQ 推送，实时消费） |
| 数据来源 | Redis Set 脏标记 + 全量扫描 | 事件消息中携带完整上下文 |
| 延迟 | 5~15 分钟 | 秒级（Consumer 实时消费） |
| 可靠性 | Redis 宕机 = 数据丢失 | MQ 持久化 + 手动 ACK = 不丢失 |
| 幂等性 | 依赖 INSERT IGNORE | 消息携带唯一 ID + 幂等表 |

```go
// internal/consumer/manager.go

type ConsumerManager struct {
    consumers []EventConsumer
    wg        sync.WaitGroup
    ctx       context.Context
    cancel    context.CancelFunc
}

type EventConsumer interface {
    Name() string
    Start(ctx context.Context) error  // 阻塞消费，ctx 取消时退出
}

// 优雅停机流程：
// 1. main.go 收到 SIGTERM/SIGINT
// 2. cancel() 通知所有 Consumer 的 ctx
// 3. Consumer 完成当前正在处理的消息 → ACK → 退出
// 4. wg.Wait() 等待所有 Consumer 退出
// 5. 关闭 HTTP Server
// 6. 关闭 MQ 连接、数据库连接
```

**Consumer 清单**：

| Consumer | 消费队列 | 处理逻辑 | 批量策略 |
|---|---|---|---|
| `LikeConsumer` | `like.q` | INSERT/DELETE likes 表 + UPDATE posts.like_count + 发出 event.count | 攒 50 条或 3 秒执行一批 |
| `PVConsumer` | `pv.q` | 聚合相同 post_id 的 PV → 批量 UPDATE posts.view_count | 攒 100 条或 5 秒执行一批 |
| `AuditConsumer` | `audit.q` | UPDATE posts.audit_status + is_visible + 写 audit_log + INCR feed:ver | 逐条处理（低频） |
| `CountConsumer` | `count.q` | UPDATE users 的冗余计数字段 | 攒 20 条或 10 秒执行一批 |

### 3.5 优雅停机完整流程

```go
// cmd/server/main.go

func main() {
    cfg := config.Load()
    logger := logger.Init(cfg.Log)
    masterDB, slaveDB := initMySQL(cfg.DB)
    rdb := initRedis(cfg.Redis) // Sentinel 模式

    // 初始化 EventBus（根据配置切换实现）
    var bus event.EventBus
    if cfg.MQ.Provider == "rabbitmq" {
        bus = event.NewRabbitMQBus(cfg.MQ.RabbitMQ)
    } else {
        bus = event.NewChannelBus(1000) // MVP：进程内 Channel
    }
    defer bus.Close()

    // 注册 Consumer
    consumerMgr := consumer.NewManager()
    consumerMgr.Register(consumer.NewLikeConsumer(bus, masterDB, rdb))
    consumerMgr.Register(consumer.NewPVConsumer(bus, masterDB, rdb))
    consumerMgr.Register(consumer.NewAuditConsumer(bus, masterDB, rdb))
    consumerMgr.Register(consumer.NewCountConsumer(bus, masterDB))
    consumerMgr.StartAll()

    // 构建 Gin Engine + 注册路由
    engine := setupRouter(masterDB, slaveDB, rdb, bus.Publisher(), cfg)

    // 启动 HTTP Server
    srv := &http.Server{Addr: ":8080", Handler: engine}
    go func() { srv.ListenAndServe() }()

    // 等待退出信号
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    logger.Info("Shutting down...")

    // 1. 停止 Consumer（完成当前消息 → ACK → 退出）
    consumerMgr.Shutdown()

    // 2. 停止 HTTP Server（排空请求）
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    srv.Shutdown(ctx)

    // 3. 关闭基础设施
    sqlDB, _ := masterDB.DB()
    sqlDB.Close()
    rdb.Close()

    logger.Info("Server exited cleanly")
}
```

---

## 4. 消息队列设计（EventBus 抽象 + RabbitMQ）

### 4.1 为什么引入消息队列

v2.0 的异步链路依赖 Redis List（审核回调）和 Redis Set + 定时 Worker（点赞/PV 同步），存在三个硬伤：

| 问题 | 说明 |
|---|---|
| **消息丢失** | Redis 非持久化队列。如果 Redis 宕机重启，List 和 Set 中未消费的数据全部丢失。审核回调丢失 = 帖子永远处于"待审核"状态 |
| **延迟高** | 定时 Worker 每 5~15 分钟扫描一次。用户点赞后最长 15 分钟才能在 MySQL 中查到 |
| **追踪困难** | v2.0 的 `UserCountSyncWorker` 依赖 `posts.updated_at` 追踪"最近有变化的用户"，但点赞不更新此字段，导致冗余计数无法正确同步（自查问题 #4） |

引入专业消息队列解决以上全部问题：持久化保证不丢、实时消费降低延迟、事件消息携带完整上下文实现精准更新。

### 4.2 落地策略：面向接口编程，分阶段实现

```
┌──────────────────────────────────────────────────────────────┐
│                     EventBus 接口抽象                         │
│                                                              │
│  type EventPublisher interface {                             │
│      Publish(ctx context.Context, event Event) error         │
│  }                                                           │
│                                                              │
│  type EventConsumer interface {                              │
│      Subscribe(ctx context.Context, topic string,            │
│                handler func(Event) error) error              │
│  }                                                           │
│                                                              │
│  type EventBus interface {                                   │
│      Publisher() EventPublisher                               │
│      Consumer() EventConsumer                                 │
│      Close() error                                           │
│  }                                                           │
└──────────────┬──────────────────────────┬────────────────────┘
               │                          │
    ┌──────────▼──────────┐    ┌──────────▼──────────┐
    │  ChannelBus（MVP）   │    │  RabbitMQBus（生产） │
    │                      │    │                      │
    │  Go Channel 实现     │    │  amqp091-go 实现     │
    │  进程内，零依赖       │    │  持久化 + ACK        │
    │  开发/测试用          │    │  死信队列 + 重试      │
    └──────────────────────┘    └──────────────────────┘
    
    切换方式：修改 config.yaml 中的 mq.provider 字段
    业务代码（Service / Consumer）零修改
```

### 4.3 事件类型定义

```go
// internal/event/types.go

type EventType string

const (
    EventLike   EventType = "event.like"
    EventUnlike EventType = "event.unlike"
    EventPV     EventType = "event.pv"
    EventAudit  EventType = "event.audit"
    EventCount  EventType = "event.count"
    EventPost   EventType = "event.post"
)

type Event struct {
    ID        string          `json:"id"`         // UUID，用于幂等去重
    Type      EventType       `json:"type"`
    Timestamp int64           `json:"timestamp"`   // UnixMilli
    Payload   json.RawMessage `json:"payload"`
}

// 各事件 Payload 定义

type LikeEvent struct {
    UserID   int64 `json:"user_id"`    // 点赞者
    PostID   int64 `json:"post_id"`
    AuthorID int64 `json:"author_id"`  // 帖子作者（用于冗余计数更新）
    Action   string `json:"action"`    // "like" or "unlike"
}

type PVEvent struct {
    PostID   int64  `json:"post_id"`
    ViewerID int64  `json:"viewer_id"` // 0 表示未登录
    IP       string `json:"ip"`        // 已脱敏
}

type AuditEvent struct {
    PostID      int64  `json:"post_id"`
    AuthorID    int64  `json:"author_id"`
    AuditStatus int8   `json:"audit_status"` // 1=机审通过 2=疑似 4=拒绝
    Remark      string `json:"remark"`
    RawResponse string `json:"raw_response"` // 第三方 API 原始返回
}

type CountEvent struct {
    UserID    int64  `json:"user_id"`
    CountType string `json:"count_type"` // "post_count" / "total_likes" / "follower_count" / "following_count"
    Delta     int    `json:"delta"`       // +1 or -1
}
```

### 4.4 EventBus 接口定义

```go
// internal/event/bus.go

type EventPublisher interface {
    Publish(ctx context.Context, evt Event) error
}

type EventSubscriber interface {
    // Subscribe 注册消费者。handler 返回 nil 表示消费成功（ACK），返回 error 触发重试/死信
    Subscribe(ctx context.Context, topic string, handler func(Event) error) error
}

type EventBus interface {
    Publisher() EventPublisher
    Subscriber() EventSubscriber
    Close() error
}
```

### 4.5 MVP 实现：Go Channel

```go
// internal/event/channel.go

// ChannelBus 用于 MVP 阶段的进程内事件总线
// 优点：零外部依赖，启动即用
// 缺点：进程重启消息丢失，不支持多实例消费
// 切换时机：部署到生产环境前切换为 RabbitMQBus

type ChannelBus struct {
    channels map[string]chan Event
    mu       sync.RWMutex
    bufSize  int
}

func NewChannelBus(bufSize int) *ChannelBus {
    return &ChannelBus{
        channels: make(map[string]chan Event),
        bufSize:  bufSize,
    }
}

func (b *ChannelBus) Publisher() EventPublisher { return b }
func (b *ChannelBus) Subscriber() EventSubscriber { return b }

func (b *ChannelBus) Publish(ctx context.Context, evt Event) error {
    b.mu.RLock()
    ch, ok := b.channels[string(evt.Type)]
    b.mu.RUnlock()
    if !ok {
        b.mu.Lock()
        ch = make(chan Event, b.bufSize)
        b.channels[string(evt.Type)] = ch
        b.mu.Unlock()
    }
    select {
    case ch <- evt:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}

func (b *ChannelBus) Subscribe(ctx context.Context, topic string, handler func(Event) error) error {
    b.mu.RLock()
    ch, ok := b.channels[topic]
    b.mu.RUnlock()
    if !ok {
        b.mu.Lock()
        ch = make(chan Event, b.bufSize)
        b.channels[topic] = ch
        b.mu.Unlock()
    }
    for {
        select {
        case evt := <-ch:
            if err := handler(evt); err != nil {
                // Channel 模式下消费失败直接记日志，不重试
                logger.Error("event handle failed", zap.Error(err), zap.String("event_id", evt.ID))
            }
        case <-ctx.Done():
            return nil
        }
    }
}

func (b *ChannelBus) Close() error { return nil }
```

### 4.6 生产实现：RabbitMQ

```go
// internal/event/rabbitmq.go

// RabbitMQBus 生产级事件总线
// 特性：
// - 消息持久化（durable queue + persistent message）
// - 手动 ACK（消费成功后确认，失败则 Nack + requeue）
// - 死信队列（超过重试次数的消息进入 DLQ，人工排查）
// - 连接断线自动重连

type RabbitMQBus struct {
    conn    *amqp.Connection
    channel *amqp.Channel
    config  RabbitMQConfig
}

type RabbitMQConfig struct {
    URL           string // amqp://user:pass@host:5672/
    Exchange      string // cooking.events
    ExchangeType  string // topic
    MaxRetry      int    // 3
    PrefetchCount int    // 10（每个 Consumer 预取数量）
}

// Queue 拓扑设计：
//
// Exchange: cooking.events (type: topic, durable)
//   │
//   ├── routing_key: event.like   → Queue: like.q   (durable, max-retry=3, DLQ: like.dlq)
//   ├── routing_key: event.unlike → Queue: like.q   (复用，Consumer 内部判断 action）
//   ├── routing_key: event.pv     → Queue: pv.q     (durable, max-retry=3, DLQ: pv.dlq)
//   ├── routing_key: event.audit  → Queue: audit.q  (durable, max-retry=3, DLQ: audit.dlq)
//   ├── routing_key: event.count  → Queue: count.q  (durable, max-retry=3, DLQ: count.dlq)
//   └── routing_key: event.post   → Queue: post.q   (durable, max-retry=3, DLQ: post.dlq)

func (b *RabbitMQBus) Publish(ctx context.Context, evt Event) error {
    body, err := json.Marshal(evt)
    if err != nil {
        return err
    }
    return b.channel.PublishWithContext(ctx,
        b.config.Exchange,  // exchange
        string(evt.Type),   // routing key
        false,              // mandatory
        false,              // immediate
        amqp.Publishing{
            DeliveryMode: amqp.Persistent, // 持久化
            ContentType:  "application/json",
            MessageId:    evt.ID,           // 幂等去重用
            Timestamp:    time.Now(),
            Body:         body,
        },
    )
}

func (b *RabbitMQBus) Subscribe(ctx context.Context, topic string, handler func(Event) error) error {
    queueName := topicToQueue(topic) // event.like → like.q
    
    msgs, err := b.channel.Consume(
        queueName,
        "",    // consumer tag（自动生成）
        false, // auto-ack = false（手动 ACK）
        false, // exclusive
        false, // no-local
        false, // no-wait
        nil,
    )
    if err != nil {
        return err
    }
    
    for {
        select {
        case msg, ok := <-msgs:
            if !ok {
                return nil // channel 关闭
            }
            
            var evt Event
            if err := json.Unmarshal(msg.Body, &evt); err != nil {
                msg.Nack(false, false) // 解析失败，不重入队列，进死信
                continue
            }
            
            if err := handler(evt); err != nil {
                retryCount := getRetryCount(msg.Headers)
                if retryCount >= b.config.MaxRetry {
                    msg.Nack(false, false) // 超过重试次数，进死信队列
                    logger.Error("event exceeded max retry, sent to DLQ",
                        zap.String("event_id", evt.ID),
                        zap.Int("retry_count", retryCount))
                } else {
                    msg.Nack(false, true) // 重入队列重试
                }
                continue
            }
            
            msg.Ack(false) // 消费成功，确认
            
        case <-ctx.Done():
            return nil
        }
    }
}
```

### 4.7 消费者实现示例：LikeConsumer

```go
// internal/consumer/like_consumer.go

type LikeConsumer struct {
    subscriber event.EventSubscriber
    masterDB   *gorm.DB
    rdb        *redis.Client
    publisher  event.EventPublisher // 用于发出 event.count
    
    // 批量聚合缓冲
    buffer     []LikeEvent
    bufferMu   sync.Mutex
    flushSize  int           // 攒 50 条
    flushInterval time.Duration // 或 3 秒
}

func (c *LikeConsumer) Start(ctx context.Context) error {
    // 启动定时刷新协程
    go c.flushLoop(ctx)
    
    // 订阅 like + unlike 事件
    return c.subscriber.Subscribe(ctx, "event.like", c.handle)
}

func (c *LikeConsumer) handle(evt event.Event) error {
    var payload LikeEvent
    json.Unmarshal(evt.Payload, &payload)
    
    c.bufferMu.Lock()
    c.buffer = append(c.buffer, payload)
    shouldFlush := len(c.buffer) >= c.flushSize
    c.bufferMu.Unlock()
    
    if shouldFlush {
        return c.flush()
    }
    return nil
}

func (c *LikeConsumer) flush() error {
    c.bufferMu.Lock()
    batch := c.buffer
    c.buffer = nil
    c.bufferMu.Unlock()
    
    if len(batch) == 0 {
        return nil
    }
    
    // 按 post_id 聚合
    likeDeltas := make(map[int64]int)   // post_id → delta（+N / -N）
    authorDeltas := make(map[int64]int) // author_id → delta
    
    return c.masterDB.Transaction(func(tx *gorm.DB) error {
        for _, e := range batch {
            if e.Action == "like" {
                // INSERT IGNORE（幂等）
                tx.Exec("INSERT IGNORE INTO likes (user_id, post_id) VALUES (?, ?)", e.UserID, e.PostID)
                likeDeltas[e.PostID]++
                authorDeltas[e.AuthorID]++
            } else {
                tx.Exec("DELETE FROM likes WHERE user_id = ? AND post_id = ?", e.UserID, e.PostID)
                likeDeltas[e.PostID]--
                authorDeltas[e.AuthorID]--
            }
        }
        
        // 批量更新 posts.like_count
        for postID, delta := range likeDeltas {
            tx.Exec("UPDATE posts SET like_count = GREATEST(0, CAST(like_count AS SIGNED) + ?) WHERE id = ?", delta, postID)
        }
        
        // 发出 event.count 更新作者的 total_likes
        for authorID, delta := range authorDeltas {
            c.publisher.Publish(context.Background(), event.Event{
                ID:   uuid.New().String(),
                Type: event.EventCount,
                Payload: marshal(CountEvent{
                    UserID:    authorID,
                    CountType: "total_likes",
                    Delta:     delta,
                }),
            })
        }
        
        return nil
    })
}
```

### 4.8 RabbitMQ 拓扑预定义

```json
// deploy/rabbitmq/definitions.json

{
  "exchanges": [
    {
      "name": "cooking.events",
      "vhost": "/",
      "type": "topic",
      "durable": true,
      "auto_delete": false
    },
    {
      "name": "cooking.dlx",
      "vhost": "/",
      "type": "direct",
      "durable": true,
      "auto_delete": false
    }
  ],
  "queues": [
    {
      "name": "like.q",
      "vhost": "/",
      "durable": true,
      "arguments": {
        "x-dead-letter-exchange": "cooking.dlx",
        "x-dead-letter-routing-key": "like.dlq"
      }
    },
    {
      "name": "like.dlq",
      "vhost": "/",
      "durable": true
    },
    {
      "name": "pv.q",
      "vhost": "/",
      "durable": true,
      "arguments": {
        "x-dead-letter-exchange": "cooking.dlx",
        "x-dead-letter-routing-key": "pv.dlq"
      }
    },
    {
      "name": "pv.dlq",
      "vhost": "/",
      "durable": true
    },
    {
      "name": "audit.q",
      "vhost": "/",
      "durable": true,
      "arguments": {
        "x-dead-letter-exchange": "cooking.dlx",
        "x-dead-letter-routing-key": "audit.dlq"
      }
    },
    {
      "name": "audit.dlq",
      "vhost": "/",
      "durable": true
    },
    {
      "name": "count.q",
      "vhost": "/",
      "durable": true,
      "arguments": {
        "x-dead-letter-exchange": "cooking.dlx",
        "x-dead-letter-routing-key": "count.dlq"
      }
    },
    {
      "name": "count.dlq",
      "vhost": "/",
      "durable": true
    },
    {
      "name": "post.q",
      "vhost": "/",
      "durable": true,
      "arguments": {
        "x-dead-letter-exchange": "cooking.dlx",
        "x-dead-letter-routing-key": "post.dlq"
      }
    },
    {
      "name": "post.dlq",
      "vhost": "/",
      "durable": true
    }
  ],
  "bindings": [
    { "source": "cooking.events", "vhost": "/", "destination": "like.q", "destination_type": "queue", "routing_key": "event.like" },
    { "source": "cooking.events", "vhost": "/", "destination": "like.q", "destination_type": "queue", "routing_key": "event.unlike" },
    { "source": "cooking.events", "vhost": "/", "destination": "pv.q", "destination_type": "queue", "routing_key": "event.pv" },
    { "source": "cooking.events", "vhost": "/", "destination": "audit.q", "destination_type": "queue", "routing_key": "event.audit" },
    { "source": "cooking.events", "vhost": "/", "destination": "count.q", "destination_type": "queue", "routing_key": "event.count" },
    { "source": "cooking.events", "vhost": "/", "destination": "post.q", "destination_type": "queue", "routing_key": "event.post" },
    { "source": "cooking.dlx", "vhost": "/", "destination": "like.dlq", "destination_type": "queue", "routing_key": "like.dlq" },
    { "source": "cooking.dlx", "vhost": "/", "destination": "pv.dlq", "destination_type": "queue", "routing_key": "pv.dlq" },
    { "source": "cooking.dlx", "vhost": "/", "destination": "audit.dlq", "destination_type": "queue", "routing_key": "audit.dlq" },
    { "source": "cooking.dlx", "vhost": "/", "destination": "count.dlq", "destination_type": "queue", "routing_key": "count.dlq" },
    { "source": "cooking.dlx", "vhost": "/", "destination": "post.dlq", "destination_type": "queue", "routing_key": "post.dlq" }
  ]
}
```

### 4.9 消息幂等性设计

```
问题：同一条消息可能被投递多次（网络抖动、Consumer 重启）
方案：Event.ID（UUID）+ 幂等保证

点赞：INSERT IGNORE（uk_user_post 唯一索引天然幂等）
PV：Redis 去重 Key（pv:dup:{post_id}:{viewer}）已保证，重复消息不会重复计数
审核：UPDATE ... WHERE audit_status < new_status（状态只能前进，重复更新无副作用）
计数：用 Delta（+1/-1）而非绝对值。重复消息导致多加的情况下，
      CountConsumer 每 6 小时做一次全量校正（SELECT COUNT → 对比 → 修正）
```

---

## 5. 数据库设计（MySQL）

### 5.1 设计原则

- 主键：`BIGINT UNSIGNED AUTO_INCREMENT`
- 时间字段：`DATETIME(3)` 毫秒精度（游标分页需要）
- 字符集：`utf8mb4_unicode_ci`
- 手机号加密存储：AES-GCM 加密 + SHA256 哈希索引
- 分页：全部使用游标分页（cursor-based）
- 场景标签：`TINYINT` + 应用层常量（不用 ENUM）
- 审核状态：`audit_status` 字段 + **`is_visible` 冗余字段**（★ 修复 #5）

### 5.2 建表 DDL

#### users 表

```sql
-- migrations/000001_create_users.up.sql

CREATE TABLE `users` (
  `id`               BIGINT UNSIGNED   NOT NULL AUTO_INCREMENT,
  `phone_hash`       VARCHAR(64)       NOT NULL COMMENT '手机号 SHA256 哈希，用于唯一索引查询',
  `phone_encrypted`  VARCHAR(200)      NOT NULL COMMENT '手机号 AES-GCM 加密存储',
  `nickname`         VARCHAR(50)       NOT NULL DEFAULT '',
  `avatar_url`       VARCHAR(500)      NOT NULL DEFAULT '',
  `bio`              VARCHAR(200)      NOT NULL DEFAULT '',
  `status`           TINYINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '0=正常 1=封禁',
  `post_count`       INT UNSIGNED      NOT NULL DEFAULT 0 COMMENT 'Worker 同步',
  `total_likes`      INT UNSIGNED      NOT NULL DEFAULT 0 COMMENT 'Worker 同步',
  `follower_count`   INT UNSIGNED      NOT NULL DEFAULT 0 COMMENT 'Worker 同步',
  `following_count`  INT UNSIGNED      NOT NULL DEFAULT 0 COMMENT 'Worker 同步',
  `created_at`       DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`       DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  `deleted_at`       DATETIME(3)       NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_phone_hash` (`phone_hash`),
  KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

```sql
-- migrations/000001_create_users.down.sql
DROP TABLE IF EXISTS `users`;
```

#### posts 表（★ 修复 #5：新增 is_visible）

```sql
-- migrations/000002_create_posts.up.sql

CREATE TABLE `posts` (
  `id`             BIGINT UNSIGNED   NOT NULL AUTO_INCREMENT,
  `user_id`        BIGINT UNSIGNED   NOT NULL,
  `title`          VARCHAR(100)      NOT NULL,
  `scene_tag`      TINYINT UNSIGNED  NOT NULL COMMENT '1=出租屋 2=一个人的饭 3=露营野炊 4=家庭厨房 5=快手日常 6=打包便当 7=减脂餐 8=节气节日',
  `cook_duration`  TINYINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '0=未填 1=15分钟内 2=30分钟内 3=1小时内 4=1小时以上',
  `cover_url`      VARCHAR(500)      NOT NULL DEFAULT '',
  `like_count`     INT UNSIGNED      NOT NULL DEFAULT 0,
  `view_count`     INT UNSIGNED      NOT NULL DEFAULT 0,
  `is_visible`     TINYINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '0=不可见 1=可见（审核通过时置 1）',
  `audit_status`   TINYINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '0=待审核 1=机审通过 2=机审疑似 3=人工通过 4=机审拒绝 5=人工拒绝',
  `audit_remark`   VARCHAR(200)      NOT NULL DEFAULT '',
  `created_at`     DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`     DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  `deleted_at`     DATETIME(3)       NULL,
  PRIMARY KEY (`id`),
  KEY `idx_user_created` (`user_id`, `created_at` DESC),
  KEY `idx_visible_created` (`is_visible`, `created_at` DESC),
  KEY `idx_scene_visible_created` (`scene_tag`, `is_visible`, `created_at` DESC),
  KEY `idx_audit_status` (`audit_status`, `created_at` DESC),
  FULLTEXT KEY `ft_title` (`title`) WITH PARSER ngram
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

> **★ 修复 #5 说明**：
> v2.0 的 Feed 查询条件是 `WHERE audit_status IN (1,3)`，复合索引遇到 `IN` 会做多次扫描再合并，深翻页触发 filesort。
> v2.1 新增 `is_visible` 冗余字段：审核通过（机审=1 或人工=3）时 AuditConsumer 将其置为 1，拒绝/删除时置为 0。
> Feed 查询改为 `WHERE is_visible=1 AND created_at < ?`，单值等值匹配 + 范围扫描，索引命中率 100%。

```sql
-- migrations/000002_create_posts.down.sql
DROP TABLE IF EXISTS `posts`;
```

#### scene_tag 应用层常量

```go
// internal/model/scene_tag.go

package model

const (
    SceneTagRental   int8 = 1 // 出租屋
    SceneTagSolo     int8 = 2 // 一个人的饭
    SceneTagCamping  int8 = 3 // 露营野炊
    SceneTagFamily   int8 = 4 // 家庭厨房
    SceneTagQuick    int8 = 5 // 快手日常
    SceneTagBento    int8 = 6 // 打包便当
    SceneTagDiet     int8 = 7 // 减脂餐
    SceneTagSeasonal int8 = 8 // 节气/节日
)

var SceneTagMap = map[int8]string{
    1: "rental", 2: "solo", 3: "camping", 4: "family",
    5: "quick", 6: "bento", 7: "diet", 8: "seasonal",
}

var SceneTagNameMap = map[int8]string{
    1: "出租屋", 2: "一个人的饭", 3: "露营野炊", 4: "家庭厨房",
    5: "快手日常", 6: "打包便当", 7: "减脂餐", 8: "节气/节日",
}

func IsValidSceneTag(tag int8) bool {
    _, ok := SceneTagMap[tag]
    return ok
}
```

#### post_steps / post_step_images / post_cover_images / post_ingredients 表

```sql
-- migrations/000003_create_post_steps.up.sql
CREATE TABLE `post_steps` (
  `id`          BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `post_id`     BIGINT UNSIGNED  NOT NULL,
  `step_order`  TINYINT UNSIGNED NOT NULL,
  `content`     TEXT             NOT NULL,
  `created_at`  DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  KEY `idx_post_order` (`post_id`, `step_order`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- migrations/000003_create_post_steps.down.sql
DROP TABLE IF EXISTS `post_steps`;
```

```sql
-- migrations/000004_create_post_images.up.sql
CREATE TABLE `post_step_images` (
  `id`          BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `post_id`     BIGINT UNSIGNED  NOT NULL,
  `step_id`     BIGINT UNSIGNED  NOT NULL,
  `image_url`   VARCHAR(500)     NOT NULL,
  `sort_order`  TINYINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  KEY `idx_step_id` (`step_id`),
  KEY `idx_post_id` (`post_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE `post_cover_images` (
  `id`          BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `post_id`     BIGINT UNSIGNED  NOT NULL,
  `image_url`   VARCHAR(500)     NOT NULL,
  `sort_order`  TINYINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  KEY `idx_post_id` (`post_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- migrations/000004_create_post_images.down.sql
DROP TABLE IF EXISTS `post_cover_images`;
DROP TABLE IF EXISTS `post_step_images`;
```

```sql
-- migrations/000005_create_ingredients.up.sql
CREATE TABLE `post_ingredients` (
  `id`          BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `post_id`     BIGINT UNSIGNED  NOT NULL,
  `name`        VARCHAR(50)      NOT NULL,
  `amount`      VARCHAR(50)      NOT NULL DEFAULT '',
  `sort_order`  TINYINT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  KEY `idx_post_id` (`post_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- migrations/000005_create_ingredients.down.sql
DROP TABLE IF EXISTS `post_ingredients`;
```

#### flavor_tags + post_flavor_tags 表

```sql
-- migrations/000006_create_tags.up.sql
CREATE TABLE `flavor_tags` (
  `id`         INT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `name`       VARCHAR(20)   NOT NULL,
  `category`   VARCHAR(20)   NOT NULL COMMENT 'taste / ingredient / diet',
  `sort_order` SMALLINT      NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO `flavor_tags` (`name`, `category`, `sort_order`) VALUES
('辣', 'taste', 1), ('甜', 'taste', 2), ('咸鲜', 'taste', 3),
('酸', 'taste', 4), ('清淡', 'taste', 5), ('重口', 'taste', 6),
('猪肉', 'ingredient', 10), ('牛肉', 'ingredient', 11),
('羊肉', 'ingredient', 12), ('鸡肉', 'ingredient', 13),
('海鲜', 'ingredient', 14), ('蛋类', 'ingredient', 15),
('豆腐', 'ingredient', 16), ('蔬菜', 'ingredient', 17),
('素食', 'ingredient', 18), ('主食', 'ingredient', 19),
('低卡', 'diet', 20), ('高蛋白', 'diet', 21),
('无麸质', 'diet', 22), ('纯素', 'diet', 23);

CREATE TABLE `post_flavor_tags` (
  `post_id`   BIGINT UNSIGNED  NOT NULL,
  `tag_id`    INT UNSIGNED     NOT NULL,
  PRIMARY KEY (`post_id`, `tag_id`),
  KEY `idx_tag_id` (`tag_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- migrations/000006_create_tags.down.sql
DROP TABLE IF EXISTS `post_flavor_tags`;
DROP TABLE IF EXISTS `flavor_tags`;
```

#### likes 表

```sql
-- migrations/000007_create_likes.up.sql
CREATE TABLE `likes` (
  `id`          BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `user_id`     BIGINT UNSIGNED  NOT NULL,
  `post_id`     BIGINT UNSIGNED  NOT NULL,
  `created_at`  DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_post` (`user_id`, `post_id`),
  KEY `idx_post_id` (`post_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- migrations/000007_create_likes.down.sql
DROP TABLE IF EXISTS `likes`;
```

#### follows 表

```sql
-- migrations/000008_create_follows.up.sql
CREATE TABLE `follows` (
  `id`            BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `follower_id`   BIGINT UNSIGNED  NOT NULL,
  `following_id`  BIGINT UNSIGNED  NOT NULL,
  `created_at`    DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_follower_following` (`follower_id`, `following_id`),
  KEY `idx_following_id` (`following_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- migrations/000008_create_follows.down.sql
DROP TABLE IF EXISTS `follows`;
```

#### audit_log 表

```sql
-- migrations/000009_create_audit_log.up.sql
CREATE TABLE `audit_log` (
  `id`             BIGINT UNSIGNED   NOT NULL AUTO_INCREMENT,
  `post_id`        BIGINT UNSIGNED   NOT NULL,
  `action`         TINYINT UNSIGNED  NOT NULL COMMENT '1=机审通过 2=机审疑似 3=机审拒绝 4=人工通过 5=人工拒绝',
  `operator`       VARCHAR(50)       NOT NULL DEFAULT 'system',
  `reason`         VARCHAR(500)      NOT NULL DEFAULT '',
  `raw_response`   TEXT              NULL,
  `created_at`     DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  KEY `idx_post_id` (`post_id`),
  KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- migrations/000009_create_audit_log.down.sql
DROP TABLE IF EXISTS `audit_log`;
```

### 5.3 索引策略（v2.1 更新）

| 查询场景 | SQL | 使用索引 |
|---|---|---|
| Feed 全部 | `WHERE is_visible=1 AND created_at < ? ORDER BY created_at DESC LIMIT 20` | `idx_visible_created` |
| Feed 按场景 | `WHERE scene_tag=? AND is_visible=1 AND created_at < ? ORDER BY created_at DESC LIMIT 20` | `idx_scene_visible_created` |
| 作者主页 | `WHERE user_id=? AND deleted_at IS NULL AND created_at < ? ORDER BY created_at DESC LIMIT 20` | `idx_user_created` |
| 全文搜索 | `MATCH(title) AGAINST(? IN BOOLEAN MODE)` | `ft_title` (ngram) |
| 待审核队列 | `WHERE audit_status=2 ORDER BY created_at` | `idx_audit_status` |
| 其余同 v2.0 | | |

### 5.4 游标分页通用结构

```go
// internal/model/dto/pagination.go

type CursorRequest struct {
    Cursor string `form:"cursor"` // 上一页最后一条的 created_at（ISO 8601 毫秒精度）
    Size   int    `form:"size" binding:"min=1,max=50"`
}

type CursorResponse struct {
    Items      interface{} `json:"items"`
    NextCursor string      `json:"next_cursor"` // 空字符串 = 没有更多
    HasMore    bool        `json:"has_more"`
}

// 使用：查询 size+1 条，返回 size 条 + has_more 标志
```

### 5.5 MySQL 主从配置

```ini
# deploy/mysql/master.cnf
[mysqld]
server-id = 1
log-bin = mysql-bin
binlog-format = ROW
binlog-row-image = FULL
sync-binlog = 1
gtid-mode = ON
enforce-gtid-consistency = ON
innodb-buffer-pool-size = 512M
innodb-log-file-size = 256M
innodb-flush-log-at-trx-commit = 1
max-connections = 200
character-set-server = utf8mb4
collation-server = utf8mb4_unicode_ci
ngram-token-size = 2
```

```ini
# deploy/mysql/slave.cnf
[mysqld]
server-id = 2
relay-log = relay-bin
read-only = ON
super-read-only = ON
gtid-mode = ON
enforce-gtid-consistency = ON
innodb-flush-log-at-trx-commit = 2
character-set-server = utf8mb4
collation-server = utf8mb4_unicode_ci
ngram-token-size = 2
```

### 5.6 ★ 修复 #3：MySQL 从库复制初始化脚本

```bash
#!/bin/bash
# deploy/mysql/init-slave.sh
#
# 从库首次启动后执行此脚本，建立主从复制关系。
# 前提：主库已启动且 GTID 开启。
#
# 用法：docker exec cooking-mysql-slave bash /docker-entrypoint-initdb.d/init-slave.sh

set -e

MASTER_HOST="${MASTER_HOST:-mysql-master}"
MASTER_PORT="${MASTER_PORT:-3306}"
REPL_USER="${REPL_USER:-repl}"
REPL_PASSWORD="${REPL_PASSWORD}"
ROOT_PASSWORD="${MYSQL_ROOT_PASSWORD}"

echo "[init-slave] Waiting for master to be ready..."
until mysql -h "$MASTER_HOST" -P "$MASTER_PORT" -u root -p"$ROOT_PASSWORD" -e "SELECT 1" &>/dev/null; do
    sleep 2
done

echo "[init-slave] Creating replication user on master (if not exists)..."
mysql -h "$MASTER_HOST" -P "$MASTER_PORT" -u root -p"$ROOT_PASSWORD" <<EOF
CREATE USER IF NOT EXISTS '${REPL_USER}'@'%' IDENTIFIED BY '${REPL_PASSWORD}';
GRANT REPLICATION SLAVE ON *.* TO '${REPL_USER}'@'%';
FLUSH PRIVILEGES;
EOF

echo "[init-slave] Configuring slave replication..."
mysql -u root -p"$ROOT_PASSWORD" <<EOF
STOP REPLICA;
CHANGE REPLICATION SOURCE TO
    SOURCE_HOST='${MASTER_HOST}',
    SOURCE_PORT=${MASTER_PORT},
    SOURCE_USER='${REPL_USER}',
    SOURCE_PASSWORD='${REPL_PASSWORD}',
    SOURCE_AUTO_POSITION=1;
START REPLICA;
EOF

echo "[init-slave] Checking replication status..."
mysql -u root -p"$ROOT_PASSWORD" -e "SHOW REPLICA STATUS\G" | grep -E "Replica_IO_Running|Replica_SQL_Running|Seconds_Behind"

echo "[init-slave] Done."
```

### 5.7 备份策略

| 类型 | 频率 | 工具 | 保留 |
|---|---|---|---|
| 全量备份 | 每天凌晨 3:00 | `mysqldump --single-transaction --set-gtid-purged=ON` | 7 天本地 + OSS 异地 |
| Binlog 增量 | 实时同步到 OSS | `mysqlbinlog --read-from-remote-server` | 30 天 |
| 备份验证 | 每周一次 | 从备份恢复到测试实例 | — |

---

## 6. Redis 缓存策略

### 6.1 Redis Sentinel 集群

```
部署拓扑：
  Redis Master   ×1
  Redis Slave    ×2
  Sentinel       ×3（各自独立数据卷，动态生成配置）

Go 客户端：
  go-redis/v9 FailoverClient，连接 Sentinel 集群
  自动感知 Master 切换，应用层无需重启
```

### 6.2 ★ 修复 #1/#2：Sentinel 启动脚本

```bash
#!/bin/bash
# deploy/redis/sentinel-entrypoint.sh
#
# 动态生成 sentinel.conf 并启动 Sentinel
# 解决：1. 配置文件不能挂载 :ro（Sentinel 需要写入运行时状态）
#       2. Redis 配置文件不支持 ${ENV_VAR} 语法

set -e

SENTINEL_PORT="${SENTINEL_PORT:-26379}"
REDIS_MASTER_HOST="${REDIS_MASTER_HOST:-redis-master}"
REDIS_MASTER_PORT="${REDIS_MASTER_PORT:-6379}"
REDIS_PASSWORD="${REDIS_PASSWORD:-}"
QUORUM="${SENTINEL_QUORUM:-2}"

CONFIG_FILE="/data/sentinel.conf"

cat > "$CONFIG_FILE" <<EOF
port ${SENTINEL_PORT}
dir /data

sentinel monitor cooking-master ${REDIS_MASTER_HOST} ${REDIS_MASTER_PORT} ${QUORUM}
sentinel auth-pass cooking-master ${REDIS_PASSWORD}
sentinel down-after-milliseconds cooking-master 5000
sentinel failover-timeout cooking-master 30000
sentinel parallel-syncs cooking-master 1
EOF

echo "[sentinel-entrypoint] Generated config at ${CONFIG_FILE}"
cat "$CONFIG_FILE"

exec redis-sentinel "$CONFIG_FILE"
```

### 6.3 Key 设计全表（v2.1 更新）

| 用途 | Key 格式 | 类型 | TTL | 说明 |
|---|---|---|---|---|
| JWT 黑名单 | `jwt:bl:{jti}` | String | Token 剩余有效期 | 登出/封号时写入 |
| 用户封禁 | `user:ban:{user_id}` | String | 永久 | Auth 中间件检查 |
| 短信验证码 | `sms:code:{phone_hash}` | String | 600s | 验证后 DEL |
| 短信日限制 | `sms:limit:{phone_hash}` | String | 86400s | INCR，>5 拒绝 |
| IP 短信限制 | `sms:ip:{ip_hash}` | String | 86400s | INCR，>10 拒绝 |
| Feed 版本号 | `feed:ver` | String | 永久 | 发帖/审核通过 INCR |
| Feed 缓存 | `feed:all:v{ver}:c{cursor_ts}` | String(JSON) | 300s | 版本号变则自然失效 |
| Feed 缓存（场景）| `feed:s{tag}:v{ver}:c{cursor_ts}` | String(JSON) | 300s | 同上 |
| 点赞计数 | `like:cnt:{post_id}` | String | 永久 | INCR/DECR |
| 点赞用户集合 | `like:set:{post_id}` | Set | 永久 | 判断用户是否已点赞 |
| PV 计数 | `pv:cnt:{post_id}` | String | 永久 | INCR |
| PV 去重 | `pv:dup:{post_id}:{uid_or_ip}` | String | 3600s | 存在则跳过 |
| 发布限流 | `limit:pub:{user_id}` | String | 86400s | INCR，>20 拒绝 |
| 用户信息缓存 | `user:info:{user_id}` | String(JSON) | 1800s | |
| 设备登录 | `user:dev:{user_id}` | SortedSet | 永久 | score=登录时间 |
| 热帖 Bloom | `bloom:like:{post_id}` | String(bitmap) | 永久 | 超 10 万点赞启用 |

> **v2.1 变更**：移除了 `like:dirty`、`pv:dirty`、`audit:result` 三个 Key。这些功能已由 MQ 消息队列替代。Redis 回归纯缓存 + 实时状态判断的职责，不再承担消息传递。

### 6.4 Feed 缓存版本号机制

```
feed:ver = 全局自增版本号
发帖成功 / 审核状态变更 → INCR feed:ver

读 Feed：
  1. GET feed:ver → ver
  2. GET feed:s{tag}:v{ver}:c{cursor_ts}
  3. 命中 → 返回
  4. 未命中 → MySQL → SETEX 300s → 返回

旧版本 key 自然过期（300s TTL），零 SCAN，零阻塞。
```

### 6.5 点赞流程（v2.1：Redis 即时状态 + MQ 异步持久化）

```
用户点赞 → Handler 验证登录 → Service 层：

  1. SISMEMBER like:set:{post_id} {user_id}
     → 已点赞：返回幂等成功
     → 未点赞：继续

  2. Redis Pipeline {
       SADD like:set:{post_id} {user_id}    // 即时状态（判重用）
       INCR like:cnt:{post_id}               // 即时计数（展示用）
     }

  3. publisher.Publish(event.Like{
       UserID: uid, PostID: pid, AuthorID: authorID, Action: "like"
     })
     // 消息投递到 MQ（或 Channel），Consumer 异步写 MySQL

  4. 返回成功响应（< 200ms）

LikeConsumer 消费后：
  → INSERT IGNORE likes 表
  → UPDATE posts.like_count
  → 发出 event.count（更新作者 total_likes）

Redis 故障降级（不变）：
  直接写 MySQL + 返回 MySQL COUNT 作为点赞数
```

### 6.6 滑动窗口限流

```go
// Redis Sorted Set 实现滑动窗口限流
// 精确控制任意滑动时间窗口内的请求数

func CheckRateLimit(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, error) {
    now := time.Now().UnixMilli()
    windowStart := now - window.Milliseconds()
    
    pipe := rdb.Pipeline()
    pipe.ZRemRangeByScore(ctx, key, "0", strconv.FormatInt(windowStart, 10))
    countCmd := pipe.ZCard(ctx, key)
    pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: now})
    pipe.Expire(ctx, key, window+time.Second)
    
    _, err := pipe.Exec(ctx)
    if err != nil {
        return true, nil // Redis 故障时放行
    }
    return countCmd.Val() < int64(limit), nil
}
```

---

## 7. RESTful API 设计规范

### 7.1 URL 设计

```
基础路径：/api/v1

认证：
  POST   /api/v1/auth/send-code
  POST   /api/v1/auth/register
  POST   /api/v1/auth/login
  POST   /api/v1/auth/logout
  POST   /api/v1/auth/refresh

用户：
  GET    /api/v1/users/:id
  GET    /api/v1/users/me
  PUT    /api/v1/users/me

内容：
  POST   /api/v1/posts
  GET    /api/v1/posts/:id
  PUT    /api/v1/posts/:id
  DELETE /api/v1/posts/:id

Feed（游标分页）：
  GET    /api/v1/feed?scene=1&cursor=2026-04-10T12:00:00.000Z&size=20
  GET    /api/v1/feed/following?cursor=...&size=20

互动：
  POST   /api/v1/posts/:id/like
  DELETE /api/v1/posts/:id/like

搜索：
  GET    /api/v1/search?q=红烧肉&scene=1&cursor=...&size=20

关注：
  POST   /api/v1/users/:id/follow
  DELETE /api/v1/users/:id/follow
  GET    /api/v1/users/:id/followers?cursor=...&size=20
  GET    /api/v1/users/:id/following?cursor=...&size=20

图片上传：
  POST   /api/v1/upload/presign
  POST   /api/v1/upload/callback

健康检查：
  GET    /health
  GET    /readiness

监控：
  GET    /metrics
```

### 7.2 统一响应格式

```json
{
    "code": 0,
    "msg": "ok",
    "request_id": "req-a1b2c3d4",
    "data": { ... }
}
```

### 7.3 错误码规范

| 错误码 | HTTP | 含义 |
|---|---|---|
| 0 | 200 | 成功 |
| 40001 | 400 | 参数校验失败 |
| 40002 | 400 | 验证码错误或已过期 |
| 40003 | 400 | 手机号格式错误 |
| 40004 | 400 | 内容包含违规词 |
| 40005 | 400 | 该手机号已注册 |
| 40100 | 401 | 未登录 / Token 过期 |
| 40101 | 401 | 账号已封禁 |
| 40102 | 401 | 已在其他设备登录 |
| 40300 | 403 | 无操作权限 |
| 40400 | 404 | 资源不存在 |
| 40401 | 404 | 内容不存在或已被删除 |
| 40402 | 404 | 用户不存在 |
| 42200 | 422 | 今日发布次数已达上限 |
| 42201 | 422 | 今日验证码发送次数已达上限 |
| 42900 | 429 | 请求过于频繁 |
| 50000 | 500 | 服务器内部错误 |
| 50001 | 500 | 数据库错误 |
| 50002 | 500 | Redis 错误（已降级） |
| 50003 | 500 | 外部服务错误 |

### 7.4 认证规范

JWT Payload：`{uid, did, jti, exp, iat}`

Auth 中间件：解析 JWT → 检查黑名单 → 检查封禁 → 检查设备数 → 注入 ctx。

### 7.5 健康检查

```go
// GET /health → 200 {"status": "ok"}（存活探针）
// GET /readiness → 200 or 503（就绪探针，检查 MySQL + Redis + MQ 连通性）
```

---

## 8. 图片上传方案（OSS 直传）

与 v2.0 一致，不再重复。核心要点：

- 客户端请求预签名 URL → Go 签发 STS 临时凭证 → 客户端直传 OSS → OSS 回调 Go
- Go 服务器零图片流量
- 图片缩略图通过 OSS 图片处理参数生成，非 Go 处理

---

## 9. 内容审核流程

### 9.1 审核状态机（含 is_visible 联动）

```
用户发布 → audit_status=0, is_visible=0
             │
     异步调用内容安全 API → 发出 event.audit
             │
    ┌────────┼────────┐
    ▼        ▼        ▼
  通过(1)  疑似(2)  拒绝(4)
  is_visible=1       is_visible=0
  前台可见   │       通知作者
             │
        人工审核
        ┌────┴────┐
        ▼         ▼
      通过(3)   拒绝(5)
  is_visible=1  is_visible=0
  前台可见     通知作者
```

AuditConsumer 在更新 `audit_status` 的同时更新 `is_visible`，保证两个字段一致。

---

## 10. 安全合规设计

与 v2.0 一致：手机号 AES-GCM 加密 + SHA256 索引、日志脱敏、安全响应头、双层限流、CORS 白名单。

---

## 11. 可观测性体系

### 11.1 Prometheus 指标（v2.1 新增 MQ 指标）

```go
var (
    // HTTP 指标（同 v2.0）
    HTTPRequestTotal    *prometheus.CounterVec
    HTTPRequestDuration *prometheus.HistogramVec

    // 业务指标（同 v2.0）
    PostPublishTotal    *prometheus.CounterVec
    LikeOperationTotal  *prometheus.CounterVec
    SearchQueryTotal    prometheus.Counter
    SearchLatency       prometheus.Histogram

    // MQ 指标（v2.1 新增）
    MQPublishTotal = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "mq_publish_total",
            Help: "Total messages published to MQ",
        },
        []string{"event_type", "status"}, // status: success / error
    )

    MQConsumeTotal = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "mq_consume_total",
            Help: "Total messages consumed from MQ",
        },
        []string{"queue", "status"}, // status: success / error / dlq
    )

    MQConsumeDuration = promauto.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "mq_consume_duration_seconds",
            Help:    "MQ message processing duration",
            Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5},
        },
        []string{"queue"},
    )

    MQQueueDepth = promauto.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "mq_queue_depth",
            Help: "Current number of messages in queue",
        },
        []string{"queue"},
    )

    // Redis 降级指标（同 v2.0）
    RedisFallbackTotal  *prometheus.CounterVec
    ExternalCallTotal   *prometheus.CounterVec
    ExternalCallDuration *prometheus.HistogramVec
)
```

### 11.2 ★ 修复 #7：Prometheus 配置

```yaml
# deploy/prometheus/prometheus.yml

global:
  scrape_interval: 15s
  evaluation_interval: 15s

rule_files:
  - /etc/prometheus/alert_rules.yml

scrape_configs:
  - job_name: 'cooking-api'
    static_configs:
      - targets: ['app-1:8080', 'app-2:8080']
    metrics_path: /metrics
    scrape_interval: 10s

  - job_name: 'nginx'
    static_configs:
      - targets: ['nginx-exporter:9113']

  - job_name: 'mysql'
    static_configs:
      - targets: ['mysqld-exporter:9104']

  - job_name: 'redis'
    static_configs:
      - targets: ['redis-exporter:9121']

  - job_name: 'rabbitmq'
    static_configs:
      - targets: ['rabbitmq:15692']
    metrics_path: /metrics
```

### 11.3 告警规则（v2.1 新增 MQ 告警）

| 告警名 | 条件 | 级别 |
|---|---|---|
| HighErrorRate | 5xx > 5% 持续 3 分钟 | Critical |
| HighLatency | P95 > 2s 持续 5 分钟 | Warning |
| MySQLDown | readiness 失败 2 次 | Critical |
| RedisDown | Redis ping 失败 3 次 | Critical |
| RedisFallback | 降级计数 > 0 持续 1 分钟 | Warning |
| **MQQueueBacklog** | 队列深度 > 10000 持续 5 分钟 | Warning |
| **MQConsumerDown** | 消费速率 = 0 持续 3 分钟 | Critical |
| **DLQNotEmpty** | 死信队列深度 > 0 | Warning |
| AuditQueueBacklog | audit.q 深度 > 1000 | Warning |
| HighMemory | Go 进程内存 > 80% | Warning |
| DiskFull | 磁盘 > 85% | Warning |

### 11.4 ★ 修复 #7：Grafana 看板要点

```
看板名：Cooking Platform Overview

Row 1 — HTTP 概览：
  - QPS（按状态码分色）
  - P50 / P95 / P99 延迟
  - 错误率

Row 2 — 业务指标：
  - 每分钟发帖量
  - 每分钟点赞量
  - 搜索 QPS + 搜索延迟
  - 各场景 Tab 点击分布

Row 3 — MQ 指标：
  - 各队列消息发布速率
  - 各队列消费速率
  - 各队列深度
  - 消费延迟分布
  - 死信队列深度

Row 4 — 基础设施：
  - MySQL 连接数 + 慢查询数
  - Redis 内存使用 + 命中率
  - RabbitMQ 连接数 + Channel 数
  - Go 进程内存 + Goroutine 数

(完整 JSON 模板在 deploy/grafana/dashboards/cooking-platform.json，
 编码阶段根据实际指标名生成)
```

---

## 12. 降级预案与容灾设计

### 12.1 降级矩阵（v2.1 新增 MQ 降级）

| 故障 | 影响 | 降级策略 | 恢复 |
|---|---|---|---|
| Redis Master 宕机 | 缓存失效 | Sentinel 自动切换（30s） | 自动 |
| Redis 全挂 | 缓存全失效 | Feed 直读 MySQL；点赞直写 MySQL；PV 暂停 | 手动重启 |
| MySQL Slave 宕机 | 读性能下降 | 读全部路由到 Master | 修复后自动恢复 |
| MySQL Master 宕机 | 无法写入 | 手动 Slave → Master（GTID） | 5~15 分钟 |
| **RabbitMQ 宕机** | 异步链路中断 | **降级为 Go Channel（进程内）+ 定时 Worker 兜底**。业务代码通过 EventBus 接口自动切换 | 手动重启 MQ |
| **MQ 消息积压** | 异步延迟增大 | 增加 Consumer 并发数（水平扩展）；非关键队列（PV）暂停消费 | 消费完积压后恢复 |
| OSS 故障 | 图片不可用 | CDN 缓存兜底 + 占位图 | 等待恢复 |
| 短信故障 | 无法登录 | 提示"稍后重试" | 等待恢复 |
| 审核 API 故障 | 新内容无法审核 | 内容写入（audit_status=0），API 恢复后队列自动消费 | 自动 |

### 12.2 MQ 降级详细方案

```go
// RabbitMQ 宕机时的降级流程：

// 1. RabbitMQBus.Publish() 失败
// 2. 触发 fallback：切换为 ChannelBus
// 3. 记录 metrics（MQFallbackTotal++）
// 4. 同时启动备用的定时 Worker（每 5 分钟扫描一次 MySQL 中未同步的记录）
// 5. MQ 恢复后，手动切回 RabbitMQBus

// 实现方式：在 EventBus 外层包一个 FallbackBus

type FallbackBus struct {
    primary   EventBus   // RabbitMQ
    fallback  EventBus   // Channel
    useFallback atomic.Bool
}

func (b *FallbackBus) Publish(ctx context.Context, evt Event) error {
    if b.useFallback.Load() {
        return b.fallback.Publisher().Publish(ctx, evt)
    }
    err := b.primary.Publisher().Publish(ctx, evt)
    if err != nil {
        metrics.MQFallbackTotal.Inc()
        b.useFallback.Store(true)
        logger.Warn("MQ publish failed, falling back to channel", zap.Error(err))
        return b.fallback.Publisher().Publish(ctx, evt)
    }
    return nil
}
```

### 12.3 Feature Flag

```yaml
feature_flags:
  mq_provider: rabbitmq          # rabbitmq / channel
  enable_audit: true
  enable_sms_verify: true
  enable_pv_tracking: true
  enable_bloom_filter: false
  maintenance_mode: false
```

---

## 13. Docker 高可用部署方案

### 13.1 Dockerfile（多阶段构建）

```dockerfile
# deploy/Dockerfile

FROM golang:1.23-alpine AS builder
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s -X main.Version=$(git describe --tags --always)" \
    -o /app/server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata curl && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    adduser -D -u 1000 appuser
WORKDIR /app
COPY --from=builder /app/server .
COPY configs/config.prod.yaml ./configs/config.yaml
USER appuser
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1
ENTRYPOINT ["./server"]
```

### 13.2 docker-compose.prod.yml（v2.1 完整版）

```yaml
version: '3.9'

services:
  # ── Go API ×2 ──────────────────────────────────
  app-1:
    build: { context: .., dockerfile: deploy/Dockerfile }
    container_name: cooking-app-1
    restart: unless-stopped
    environment: &app-env
      - APP_ENV=production
      - DB_MASTER_HOST=mysql-master
      - DB_SLAVE_HOST=mysql-slave
      - DB_PORT=3306
      - DB_NAME=${DB_NAME}
      - DB_USER=${DB_USER}
      - DB_PASSWORD=${DB_PASSWORD}
      - REDIS_SENTINEL_ADDRS=sentinel-1:26379,sentinel-2:26379,sentinel-3:26379
      - REDIS_MASTER_NAME=cooking-master
      - REDIS_PASSWORD=${REDIS_PASSWORD}
      - MQ_PROVIDER=rabbitmq
      - MQ_URL=amqp://${RABBITMQ_USER}:${RABBITMQ_PASSWORD}@rabbitmq:5672/
      - JWT_SECRET=${JWT_SECRET}
      - AES_KEY=${AES_KEY}
      - OSS_ACCESS_KEY=${OSS_ACCESS_KEY}
      - OSS_SECRET_KEY=${OSS_SECRET_KEY}
    depends_on:
      mysql-master: { condition: service_healthy }
      redis-master: { condition: service_healthy }
      rabbitmq: { condition: service_healthy }
    networks: [cooking-net]

  app-2:
    build: { context: .., dockerfile: deploy/Dockerfile }
    container_name: cooking-app-2
    restart: unless-stopped
    environment: *app-env
    depends_on:
      mysql-master: { condition: service_healthy }
      redis-master: { condition: service_healthy }
      rabbitmq: { condition: service_healthy }
    networks: [cooking-net]

  # ── MySQL 主从 ──────────────────────────────────
  mysql-master:
    image: mysql:8.0
    container_name: cooking-mysql-master
    restart: unless-stopped
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD}
      MYSQL_DATABASE: ${DB_NAME}
      MYSQL_USER: ${DB_USER}
      MYSQL_PASSWORD: ${DB_PASSWORD}
    volumes:
      - mysql-master-data:/var/lib/mysql
      - ./mysql/master.cnf:/etc/mysql/conf.d/master.cnf:ro
    healthcheck:
      test: ["CMD", "mysqladmin", "ping", "-h", "localhost", "-u", "root", "-p${MYSQL_ROOT_PASSWORD}"]
      interval: 10s
      timeout: 5s
      retries: 5
    networks: [cooking-net]

  mysql-slave:
    image: mysql:8.0
    container_name: cooking-mysql-slave
    restart: unless-stopped
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD}
      MASTER_HOST: mysql-master
      REPL_USER: repl
      REPL_PASSWORD: ${REPL_PASSWORD}
    volumes:
      - mysql-slave-data:/var/lib/mysql
      - ./mysql/slave.cnf:/etc/mysql/conf.d/slave.cnf:ro
      - ./mysql/init-slave.sh:/docker-entrypoint-initdb.d/init-slave.sh:ro
    depends_on:
      mysql-master: { condition: service_healthy }
    networks: [cooking-net]

  # ── Redis Master + Slave ×2 ────────────────────
  redis-master:
    image: redis:7.2-alpine
    container_name: cooking-redis-master
    restart: unless-stopped
    command: >
      redis-server --requirepass ${REDIS_PASSWORD}
      --maxmemory 512mb --maxmemory-policy allkeys-lru
      --save 900 1 --save 300 10 --appendonly yes
    volumes: [redis-master-data:/data]
    healthcheck:
      test: ["CMD", "redis-cli", "-a", "${REDIS_PASSWORD}", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
    networks: [cooking-net]

  redis-slave-1:
    image: redis:7.2-alpine
    container_name: cooking-redis-slave-1
    restart: unless-stopped
    command: >
      redis-server --requirepass ${REDIS_PASSWORD} --masterauth ${REDIS_PASSWORD}
      --replicaof redis-master 6379 --maxmemory 512mb --maxmemory-policy allkeys-lru
    volumes: [redis-slave-1-data:/data]
    depends_on: [redis-master]
    networks: [cooking-net]

  redis-slave-2:
    image: redis:7.2-alpine
    container_name: cooking-redis-slave-2
    restart: unless-stopped
    command: >
      redis-server --requirepass ${REDIS_PASSWORD} --masterauth ${REDIS_PASSWORD}
      --replicaof redis-master 6379 --maxmemory 512mb --maxmemory-policy allkeys-lru
    volumes: [redis-slave-2-data:/data]
    depends_on: [redis-master]
    networks: [cooking-net]

  # ── Sentinel ×3（★ 修复 #1/#2）────────────────
  sentinel-1:
    image: redis:7.2-alpine
    container_name: cooking-sentinel-1
    restart: unless-stopped
    entrypoint: ["/bin/sh", "/scripts/sentinel-entrypoint.sh"]
    environment:
      REDIS_PASSWORD: ${REDIS_PASSWORD}
      REDIS_MASTER_HOST: redis-master
    volumes:
      - ./redis/sentinel-entrypoint.sh:/scripts/sentinel-entrypoint.sh:ro
      - sentinel-1-data:/data
    depends_on: [redis-master]
    networks: [cooking-net]

  sentinel-2:
    image: redis:7.2-alpine
    container_name: cooking-sentinel-2
    restart: unless-stopped
    entrypoint: ["/bin/sh", "/scripts/sentinel-entrypoint.sh"]
    environment:
      REDIS_PASSWORD: ${REDIS_PASSWORD}
      REDIS_MASTER_HOST: redis-master
    volumes:
      - ./redis/sentinel-entrypoint.sh:/scripts/sentinel-entrypoint.sh:ro
      - sentinel-2-data:/data
    depends_on: [redis-master]
    networks: [cooking-net]

  sentinel-3:
    image: redis:7.2-alpine
    container_name: cooking-sentinel-3
    restart: unless-stopped
    entrypoint: ["/bin/sh", "/scripts/sentinel-entrypoint.sh"]
    environment:
      REDIS_PASSWORD: ${REDIS_PASSWORD}
      REDIS_MASTER_HOST: redis-master
    volumes:
      - ./redis/sentinel-entrypoint.sh:/scripts/sentinel-entrypoint.sh:ro
      - sentinel-3-data:/data
    depends_on: [redis-master]
    networks: [cooking-net]

  # ── RabbitMQ（v2.1 新增）────────────────────────
  rabbitmq:
    image: rabbitmq:3.13-management-alpine
    container_name: cooking-rabbitmq
    restart: unless-stopped
    environment:
      RABBITMQ_DEFAULT_USER: ${RABBITMQ_USER}
      RABBITMQ_DEFAULT_PASS: ${RABBITMQ_PASSWORD}
    ports:
      - "15672:15672"     # 管理面板（生产环境仅内网访问）
    volumes:
      - rabbitmq-data:/var/lib/rabbitmq
      - ./rabbitmq/definitions.json:/etc/rabbitmq/definitions.json:ro
      - ./rabbitmq/rabbitmq.conf:/etc/rabbitmq/rabbitmq.conf:ro
    healthcheck:
      test: ["CMD", "rabbitmq-diagnostics", "check_running"]
      interval: 15s
      timeout: 10s
      retries: 5
    networks: [cooking-net]

  # ── Nginx ──────────────────────────────────────
  nginx:
    image: nginx:1.27-alpine
    container_name: cooking-nginx
    restart: unless-stopped
    ports: ["80:80", "443:443"]
    volumes:
      - ./nginx/nginx.conf:/etc/nginx/nginx.conf:ro
      - ./nginx/upstream.conf:/etc/nginx/conf.d/upstream.conf:ro
      - ./nginx/certs:/etc/nginx/certs:ro
    depends_on: [app-1, app-2]
    networks: [cooking-net]

  # ── Prometheus ─────────────────────────────────
  prometheus:
    image: prom/prometheus:v2.51.0
    container_name: cooking-prometheus
    restart: unless-stopped
    volumes:
      - ./prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - prometheus-data:/prometheus
    networks: [cooking-net]

  # ── Grafana ────────────────────────────────────
  grafana:
    image: grafana/grafana:10.4.0
    container_name: cooking-grafana
    restart: unless-stopped
    environment:
      GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_PASSWORD}
    ports: ["3000:3000"]
    volumes:
      - grafana-data:/var/lib/grafana
      - ./grafana/dashboards:/etc/grafana/provisioning/dashboards:ro
    networks: [cooking-net]

volumes:
  mysql-master-data:
  mysql-slave-data:
  redis-master-data:
  redis-slave-1-data:
  redis-slave-2-data:
  sentinel-1-data:
  sentinel-2-data:
  sentinel-3-data:
  rabbitmq-data:
  prometheus-data:
  grafana-data:

networks:
  cooking-net:
    driver: bridge
```

### 13.3 RabbitMQ 配置

```conf
# deploy/rabbitmq/rabbitmq.conf

# 加载预定义拓扑（Exchange/Queue/Binding）
management.load_definitions = /etc/rabbitmq/definitions.json

# Prometheus 指标端点
prometheus.return_per_object_metrics = true

# 内存限制
vm_memory_high_watermark.relative = 0.6

# 磁盘限制
disk_free_limit.absolute = 1GB

# 连接心跳
heartbeat = 60
```

### 13.4 Nginx 配置

与 v2.0 一致：upstream least_conn 负载均衡、SSL 终止、双层限流、安全响应头、JSON 结构化日志。

---

## 14. CI/CD 流水线

与 v2.0 一致，新增 RabbitMQ service 到 CI test job：

```yaml
# .github/workflows/ci.yml 中新增：
services:
  # ... mysql, redis（同 v2.0）
  rabbitmq:
    image: rabbitmq:3.13-alpine
    ports:
      - 5672:5672
    options: >-
      --health-cmd="rabbitmq-diagnostics check_running"
      --health-interval=10s
      --health-timeout=5s
      --health-retries=5
```

CD 滚动部署流程不变。

---

## 15. 数据库迁移管理

与 v2.0 一致：`golang-migrate`，up/down 版本化，CI 自动测试。

---

## 16. 关键技术决策记录（ADR）

### ADR-001 ~ ADR-010

与 v2.0 一致（gin、GORM、Feed 不推荐、点赞最终一致、MySQL FULLTEXT、游标分页、OSS 直传、TINYINT、手机号加密、版本号缓存）。

### ADR-011：选择 RabbitMQ 而非 Kafka / RocketMQ / Redis Stream

**决策**：使用 RabbitMQ 作为消息队列。

**备选方案对比**：

| 维度 | RabbitMQ | Kafka | RocketMQ | Redis Stream |
|---|---|---|---|---|
| 运维复杂度 | 低（单节点即可用） | 高（ZK + Broker 集群） | 中（NameServer + Broker） | 最低（已有 Redis） |
| 消息可靠性 | 持久化 + ACK + 死信队列 | 高（副本机制） | 高 | 低（AOF 可能丢） |
| 延迟 | 毫秒级 | 毫秒级 | 毫秒级 | 毫秒级 |
| 吞吐量 | 万级/秒 | 百万级/秒 | 十万级/秒 | 十万级/秒 |
| 内存占用 | ~300MB | ~1GB+ | ~500MB+ | 已有 |
| 社区生态 | 成熟，Go 客户端稳定 | 成熟 | 阿里系为主 | 客户端不够成熟 |
| 面试加分 | ★★★★ | ★★★★★ | ★★★★ | ★★ |

**选择 RabbitMQ 的理由**：
1. **运维简单**：单节点 + management 插件即可生产运行，两人团队可维护
2. **可靠性足够**：持久化队列 + 手动 ACK + 死信队列，覆盖审核回调等关键链路
3. **吞吐量匹配**：万级/秒的吞吐量完全覆盖中期目标（10 万 DAU），不需要 Kafka 的百万级吞吐
4. **成本可控**：单节点 ~300MB 内存，不显著增加服务器成本

**迁移触发条件**：当日消息量超过 1000 万条 **或** 需要消息回溯/流式处理时，评估迁移到 Kafka。

### ADR-012：EventBus 接口抽象 + 分阶段实现

**决策**：定义 `EventPublisher` / `EventSubscriber` 接口，MVP 用 Go Channel 实现，生产用 RabbitMQ 实现。

**理由**：
1. **开发效率**：MVP 阶段用 Channel 跑通主流程，零外部依赖，快速看到前端界面运转
2. **无缝切换**：接口不变，业务代码零修改，只需改 config 中的 `mq_provider` 字段
3. **面试价值**：能向面试官展示"面向接口编程"的工程素养，比直接硬编码 MQ 更有含金量

### ADR-013：is_visible 冗余字段替代 audit_status IN 查询

**决策**：posts 表新增 `is_visible TINYINT` 冗余字段。

**理由**：v2.0 的 Feed 查询 `WHERE audit_status IN (1,3)` 在复合索引上会做多次扫描再合并。`is_visible=1` 是单值等值匹配，索引命中率 100%，查询性能稳定。冗余字段由 AuditConsumer 在更新审核状态时联动更新，一致性有 MQ 保证。

---

## 附录 A：扩容路径地图

| 阶段 | DAU | 核心瓶颈 | 操作 |
|---|---|---|---|
| MVP | 0~1 万 | 无 | 当前架构 |
| 早期 | 1~10 万 | MySQL 读压力 | 增加从库；Go 实例 2→4 |
| 中期 | 10~50 万 | likes 表、搜索延迟 | likes 分表；评估 ES |
| 增长 | 50~100 万 | 全链路 QPS | PolarDB；Redis Cluster；RabbitMQ 集群/镜像队列 |
| 规模化 | 100 万+ | 服务耦合 | 微服务拆分；MQ 迁移到 Kafka；引入 ES |

---

## 附录 B：v2.0 → v2.1 完整差异速查

| 维度 | v2.0 | v2.1 |
|---|---|---|
| 消息队列 | 无（Redis List + Set 模拟） | RabbitMQ（持久化 + ACK + 死信） |
| 异步架构 | 定时 Worker 轮询 | EventBus 事件驱动 + Consumer |
| 落地策略 | 直接实现 | 接口抽象（MVP: Channel → 生产: RabbitMQ） |
| Feed 查询 | `audit_status IN (1,3)` | `is_visible=1`（单值索引） |
| 冗余计数同步 | 依赖 updated_at（有 bug） | MQ 事件驱动（精准更新） |
| Sentinel 配置 | 静态文件 `:ro` 挂载（启动失败） | entrypoint 脚本动态生成 |
| MySQL 从库 | 缺少复制初始化 | init-slave.sh 自动配置 |
| Prometheus 配置 | 空引用 | 完整 scrape config |
| Grafana 看板 | 空引用 | 完整看板设计 |
| migrations down | 部分缺失 | 全部补全 |
| Docker 容器数 | 13 个 | 15 个（+RabbitMQ +独立 sentinel 卷） |

---

*文档版本：v2.1（草稿）*
*下一阶段：详细设计（阶段 4）——每个模块的接口定义与核心逻辑伪代码，从用户模块开始*
