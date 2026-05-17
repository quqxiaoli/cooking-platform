# PRD v3.0 · 烹饪内容分享平台 · 最终技术文档

> **生成时间**：2026-05-17  
> **生成依据**：Step 1–17 所有进度追踪偏离点 + 架构体检报告 + 代码变更清单 + 故事线 + Step 13–17 开发规范  
> **权威性**：自本文生效起，本文取代 v2.1 / v2.2 PRD，成为唯一权威参考。  
> **用途**：面试系统讲解 / 后续迭代的起点 / Step 18–20 上线规划的输入。

---

## §1 项目定位与功能边界

**产品定位**：面向公网上线的商业级烹饪内容分享平台，对标字节/腾讯产品研发规范。  
**核心气质**：松弛、真实、有温度、可复现、有场景感、不焦虑。  
**目标用户**：22–28 岁独居/合租年轻人。

### 功能边界（MVP）

| 模块 | 状态 | 说明 |
|------|------|------|
| 用户系统（注册/登录/手机号） | ✅ | AES-GCM 加密 + SHA256 索引 |
| 内容发布（帖子/步骤/图片） | ✅ | OSS PresignedURL 直传 |
| Feed 流（按 scene_tag 分页） | ✅ | 游标分页 + Redis 版本号缓存 |
| 内容审核 | ✅ | 阿里云 Green API（异步 Consumer） |
| 点赞 | ✅ | Redis Set + Lua 原子操作 + MQ 异步落库 |
| 浏览量（PV） | ✅ | 24h 去重 + MQ 异步聚合 |
| 搜索 | ✅ | MySQL FULLTEXT（ngram_token_size=2） |
| 关注 | ✅ | 关注/取关/列表 |
| 图片上传 | ✅ | PresignedURL + nonce 防重放 |
| 评论 | ❌ | 永久排除 |
| 视频 | ❌ | MVP 不做 |

---

## §2 系统架构

### 2.1 总体拓扑

```
Internet
    │
  [Nginx] ─── upstream round-robin ─── [app1:8080] ─── [MySQL master:3306]
    │                               └── [app2:8080] ─── [MySQL slave:3307]
    │
  /metrics (内网不对外)                  │
  Prometheus ←─ scrape ─────────────── [app1/app2 /metrics]
  Grafana ←─ datasource ──────────────  Prometheus

  [app1/app2] ── AMQP ──────────────── [RabbitMQ:5672]
  [app1/app2] ── TCP ────────────────── [Redis Sentinel:26379/6379]
```

### 2.2 进程模型

- **双 Go 实例**（app1:8080, app2:8080）：无状态，Nginx round-robin 分发
- **Nginx**：反向代理 + `proxy_next_upstream`（被动健康检查）
- **MySQL 主从**（GTID 模式）：写 → master，读 → slave（DBResolver 自动路由）
- **Redis**：Sentinel 模式（master + 2 sentinels），数据结构见 §6.3
- **RabbitMQ**：单 broker，Exchange `cooking.events`（topic，durable），生产消息持久化（DeliveryMode=Persistent）
- **Prometheus**：pull-based，scrape 间隔 15s
- **Grafana**：auto-provisioning（datasource + dashboard JSON），端口 3000

### 2.3 请求路径

```
Client → Nginx(:80) → app{1|2}:8080
  └─ /api/v1/*  → Gin Router
      ├─ Auth Middleware（JWT 验签 + 黑名单）
      ├─ Metrics Middleware（HTTP 请求计数 + 延迟直方图）
      ├─ Logger Middleware（请求日志 + 日志脱敏）
      └─ Handler → Service → Repository → MySQL/Redis
                        └─ EventBus.Publish → RabbitMQ → Consumer → MySQL
```

---

## §3 EventBus 接口与实现

### 3.1 接口定义（不可更改）

```go
// internal/event/bus.go
type EventPublisher interface {
    Publish(ctx context.Context, event Event) error
}

type EventSubscriber interface {
    Subscribe(topic string, handler func(Event)) error
}

type EventBus interface {
    EventPublisher
    EventSubscriber
    Close() error
}
```

### 3.2 两种实现对比

| 维度 | ChannelBus（dev） | RabbitMQBus（prod） |
|------|-----------------|---------------------|
| 文件 | `internal/event/channel.go` | `internal/event/rabbitmq.go` |
| 语义 | at-most-once | at-least-once（ACK/Nack） |
| 消息持久化 | ❌ 进程重启丢失 | ✅ DeliveryMode=Persistent |
| 多实例 | ❌ 进程内广播 | ✅ 命名队列，多实例共享消费 |
| 切换方式 | `config.yaml mq.provider: channel` | `mq.provider: rabbitmq` |
| 死信 | ❌ | ✅ DLX `cooking.events.dlx` |
| 自动重连 | N/A | ✅ 指数退避，最多 5 次 |

### 3.3 RabbitMQ Exchange 拓扑（生产约定，不可更改）

```
Exchange: cooking.events   type: topic   durable: true
Routing key: == event.Topic 字符串（如 "event.like"）
Queue naming: cooking.<topic>（如 cooking.event.like）
DLX Exchange: cooking.events.dlx   type: fanout   durable: true
DLX Queue: cooking.dlq             durable: true
```

> **偏离点**（Step 13）：PRD v2.2 §1.4 规定队列命名为 `cooking.<topic>.<consumer_group>`。  
> 实际用 `cooking.<topic>`，因为 `EventSubscriber` 接口不含 consumer_group 参数，强制命名 consumer_group 需改接口签名。已确认偏离，纳入本文。

### 3.4 消息格式

```go
type Event struct {
    Topic     string      `json:"topic"`
    Payload   interface{} `json:"payload"`
    CreatedAt time.Time   `json:"created_at"`
}
```

---

## §4 Consumer 设计模式

### 4.1 架构模型（双层 goroutine）

```
EventBus.Subscribe
    │
    ├─ subscribe goroutine(s)：只负责 Unmarshal + 入 eventCh（传播 backpressure）
    │
    └─ flushLoop goroutine：单消费者，三路 select
           ├─ eventCh → 积攒到 batchSize
           ├─ ticker  → 超时冲刷
           └─ ctx.Done → subWg.Wait → DrainChan → flush(context.Background)
```

### 4.2 Consumer 目录

| Consumer | 订阅 Topic | 批大小/间隔 | 落库目标 |
|----------|-----------|-----------|---------|
| LikeConsumer | event.like / event.unlike | 50 / 3s | likes 表 + posts.like_count |
| PVConsumer | event.pv | 100 / 5s | posts.view_count（GROUP BY 聚合） |
| CountConsumer | event.post / event.like / event.unlike | 20 / 10s | users 冗余计数 |
| AuditConsumer | event.audit | 1 / 即时 | audit_status + audit_log |

### 4.3 强制规则

1. **DrainChan**：停机 drain 必须调用 `consumer.DrainChan[T]`，禁止手写 labeled-for 块
2. **flush ctx**：drain 后的 flush 必须用 `context.Background()`，不复用已取消的 ctx
3. **关闭日志**：`zap.L().Info("xxx consumer drained", zap.Int("total_processed", n))`
4. **构造函数**：第三参数固定为 `cfg config.XxxConsumerConfig`

---

## §5 数据库设计

### 5.1 表清单

| 表名 | 核心字段 | 索引 |
|------|---------|------|
| users | id, phone_encrypted, phone_hash, username, avatar, like_count, post_count, follow_count, follower_count | uk_phone_hash, idx_username |
| posts | id, user_id, title, content, scene_tag, cover_url, image_urls, like_count, view_count, is_visible, audit_status | idx_user_id, idx_scene_tag_created, ft_title_content(FULLTEXT) |
| post_steps | post_id, step_no, description, image_url | pk(post_id, step_no) |
| likes | user_id, post_id, created_at | uk(user_id,post_id), idx_post_id |
| follows | follower_id, following_id, created_at | uk(follower_id,following_id), idx_following_id |
| audit_logs | post_id, status, reason, provider, created_at | idx_post_id |

### 5.2 MySQL 主从配置（GTID）

```
master: server-id=1, gtid-mode=ON, enforce-gtid-consistency=ON, binlog-format=ROW
slave:  server-id=2, gtid-mode=ON, read_only=ON
```

- Migration 只在 master 执行（`make migrate-up`）
- GORM DBResolver：写→master，读→slave（全部 SELECT 自动路由）

> **偏离点**（Step 14）：`super_read_only` 由 init-slave.sh 运行时 SET GLOBAL 设置，而非写入 slave.cnf，原因：Docker entrypoint 时序问题。

---

## §6 Redis 设计

### 6.1 部署模式

Redis Sentinel（1 master + 2 sentinel），go-redis/v9 UniversalClient 自动故障切换。

### 6.2 Lua 原子操作规范

凡"读后写"或"条件写"必须用 Lua 脚本：

| 场景 | 位置 | 脚本说明 |
|------|------|---------|
| SADD + 条件 INCR | `like_cache.go: addLikeScript` | SADD 返回 0 时跳过 INCR |
| DECR + 钳位 0 | `like_cache.go: decrClampScript` | GET 为空或 ≤0 时 SET 0 |

### 6.3 Redis Key 命名全表

| 用途 | Key 模板 | TTL | 注意 |
|------|---------|-----|------|
| 验证码 | `sms:code:{phone_hash}` | 5m | |
| SMS 间隔（60s） | `sms:window:{phone_hash}` | 60s | |
| SMS 日限（手机） | `sms:limit:{phone_hash}` | 24h | |
| SMS 日限（IP） | `sms:ip:{ip_hash}` | 24h | |
| JWT 黑名单 | `jwt:bl:{jti}` | access token 剩余 TTL | |
| 用户信息缓存 | `user:info:{user_id}` | 30m | |
| Feed 版本号 | `feed:ver` | 永久 | INCR 触发缓存失效 |
| Feed 缓存 | `feed:s{scene}:v{ver}:c{cursorMs}` | 5m | cfg.Cache.FeedCacheTTL |
| PV 去重（用户） | `pv:dup:{post_id}:u{user_id}` | 1h | cfg.Cache.PVDedupTTL |
| PV 去重（IP） | `pv:dup:{post_id}:i{ip_hash}` | 1h | |
| 点赞限流 | `limit:like:{user_id}` | 24h | |
| 点赞 SET | `like:set:{post_id}` | 7d（滑动） | cfg.Cache.LikeStateTTL |
| 点赞计数器 | `like:cnt:{post_id}` | 7d（滑动） | |
| 上传 nonce | `upload:nonce:{nonce}` | 15m | GETDEL 防重放 |

---

## §7 错误码全表

格式：`XYYZZZ`（X=类别，YY=子模块，ZZZ=序号）

| 段位 | 范围 | 核心错误码 |
|------|------|-----------|
| 通用 | 400000–409999 | 400001(参数), 401001-003(Token), 403001(权限), 404001(资源), 429001(限流), 500001-003(Server/DB/Redis), 503001(不可用) |
| 用户 | 410000–419999 | 410101(手机号格式), 410102(验证码错误), 410103(验证码过期), 410104(用户不存在), 410105(用户已存在), 410106(用户名长度), 410107(头像URL非法), 410108(发送过于频繁), 410109(SMS发送失败), 410110(密码错误), 410111(Token刷新失败), 410112(用户名格式) |
| 内容 | 420000–429999 | 412101(标题长度), 412102(scene_tag非法), 412103(内容长度), 412104(帖子不存在), 412105(步骤缺失), 412106(步骤描述长度), 412107(分页参数), 412108(无权操作) |
| 互动 | 430000–439999 | 含点赞/浏览量相关 |
| 关注 | 440000–449999 | 440101(不能自关注), 440102(已关注), 440103(关注不存在), 440104(关注列表参数) |
| 搜索 | 450000–459999 | 450101(关键词长度), 450102(游标格式) |
| 上传 | 460000–469999 | 460101(文件类型), 460102(文件大小), 460103(presign失败), 460104(callback nonce无效), 460105(URL白名单), 460106(nonce已使用) |
| 审核 | 470000–479999 | 470101(API调用失败), 470102(写入失败)——仅内部使用 |
| 手机号加密 | 480000–489999 | 480101(加密失败), 480102(解密失败)——仅内部使用 |

---

## §8 配置项全表

所有可调参数必须定义在 `pkg/config/config.go`（Config-First 规则），禁止硬编码。

### 8.1 核心配置结构

```go
type Config struct {
    Server     ServerConfig
    Database   DatabaseConfig
    Redis      RedisConfig
    JWT        JWTConfig
    MQ         MQConfig
    SMS        SMSConfig
    OSS        OSSConfig
    Audit      AuditConfig
    Encryption EncryptionConfig
    Consumer   ConsumerConfig   // 含 Like/PV/Count 三个子结构
    Cache      CacheConfig
    Metrics    MetricsConfig
    Ratelimit  RatelimitConfig
}
```

### 8.2 关键参数与生产建议值

| 配置路径 | Dev 默认值 | 生产建议值 | 说明 |
|---------|-----------|-----------|------|
| server.port | 8080 | 8080 | |
| database.max_open_conns | 10 | 50 | 根据 MySQL max_connections 调整 |
| database.max_idle_conns | 5 | 10 | |
| mq.provider | channel | rabbitmq | 生产必须改 |
| mq.reconnect_max_retries | 5 | 10 | 生产网络抖动更频繁 |
| consumer.like.batch_size | 50 | 100 | 高峰期适当调大 |
| consumer.like.flush_interval | 3s | 2s | 减少延迟 |
| consumer.pv.batch_size | 100 | 200 | |
| cache.like_state_ttl | 7d | 7d | |
| cache.pv_dedup_ttl | 1h | 1h | |
| oss.presign_ttl | 15m | 15m | |
| oss.upload_hourly | 30 | 30 | 限流阈值 |
| metrics.enabled | true | true | |
| metrics.namespace | cooking | cooking | |

---

## §9 监控 SLO

### 9.1 已接入指标

| 优先级 | 指标名（prometheus） | 采集位置 |
|--------|---------------------|---------|
| P0 | `cooking_http_requests_total{handler,method,status}` | Gin middleware |
| P0 | `cooking_http_request_duration_seconds{handler,method,status}` | Gin middleware（histogram） |
| P0 | `cooking_consumer_events_processed_total{consumer,topic}` | Consumer flushLoop |
| P0 | `cooking_consumer_queue_depth{consumer,topic}` | Consumer ticker |
| P1 | `cooking_redis_command_duration_seconds{command}` | go-redis Hook |
| P1 | `cooking_mysql_pool_open_connections` / `idle` / `in_use` | sql.DBStats Collector |
| P2 | Go runtime（GC pause / goroutine count）| prometheus 默认注册 |

### 9.2 SLO 目标（建议，待压测校准）

| 指标 | 目标 | 告警阈值 |
|------|------|---------|
| HTTP P95 延迟 | < 200ms | > 500ms 持续 5min |
| HTTP P99 延迟 | < 1s | > 2s 持续 3min |
| HTTP 错误率（5xx） | < 0.1% | > 1% 持续 2min |
| Consumer 队列积压 | < batchSize × 2 | > batchSize × 10 持续 5min |
| Redis P95 命令延迟 | < 10ms | > 50ms 持续 3min |

### 9.3 Grafana Dashboard

文件路径：`deploy/grafana/dashboards/cooking-platform.json`  
6 个 Panel：HTTP 请求速率、HTTP P95 延迟、Consumer 处理速率、Consumer 队列积压、Redis 命令延迟、MySQL 连接池使用率。

---

## §10 部署 Runbook

### 10.1 Docker Compose 启动顺序

```bash
# 1. 启动基础设施（含健康检查等待）
make docker-up

# 2. 执行 schema migration（只在 master）
make migrate-up

# 3. 启动应用（docker-compose 会自动随 docker-up 启动 app1/app2）
# 若需单独重启应用：
docker compose restart app1 app2
```

### 10.2 配置热更新

所有配置通过 `CONFIG_PATH` 环境变量选择文件，重启生效。Redis/DB 连接需重启。

### 10.3 回滚流程

```bash
# 1. 回滚 DB migration
make migrate-down

# 2. 回滚应用（切回上一个 Git tag）
git checkout <prev-tag>
docker compose build app1 app2
docker compose up -d app1 app2
```

### 10.4 RabbitMQ 运维

- Management UI：`http://localhost:15672`（cooking/cooking123）
- 查看 DLQ：`cooking.dlq` 队列中的消息为多次 Nack 后积压的失败消息
- 手动重投：通过 Management UI 将 DLQ 消息 Move 回原队列

### 10.5 CI/CD 发布流程

```
git tag v1.0.0
git push origin v1.0.0
→ GitHub Actions release.yml 触发
→ 构建 Docker 镜像
→ 推送到 ghcr.io/quqxiaoli/cooking-platform:1.0.0 + :latest
```

---

## §11 安全清单

### 11.1 AES-GCM 手机号加密

- 密钥：256-bit，通过 `APP_ENCRYPTION_PHONE_KEY` 环境变量注入（64 hex 字符）
- 每次加密生成随机 12-byte nonce，存入密文前缀（防重放）
- 索引字段：`phone_hash = SHA256(phone + pepper)` 用于查询，明文不入库
- **密钥轮换**：运行 `make migrate-phone` 重加密所有手机号

### 11.2 OSS PresignedURL

- TTL：15 分钟（cfg.OSS.PresignTTL）
- nonce：UUID v4，Redis GETDEL 防重放，15min TTL 与 presign 对齐
- 服务器不过图片流量，presign URL 直传到 OSS

### 11.3 JWT

- 黑名单：退出登录时将 JTI 写入 Redis，TTL = access token 剩余有效期
- access token TTL：2h；refresh token TTL：7d

### 11.4 日志脱敏

Gin 请求日志中以下参数被替换为 `***`：
- Query 参数：`phone`, `code`, `token`（大小写不敏感）
- zap 字段：手机号、验证码等敏感字段用 `MaskPhone()` / `MaskToken()` 包裹

### 11.5 敏感配置管理

| 配置 | 存储方式 |
|------|---------|
| DB 密码 | `APP_DATABASE_DSN` 环境变量 |
| Redis 密码 | `APP_REDIS_PASSWORD` |
| JWT Secret | `APP_JWT_SECRET` |
| RabbitMQ URL | `APP_MQ_URL` |
| 阿里云 AccessKey | `APP_SMS_*` / `APP_OSS_*` / `APP_AUDIT_*` |
| 手机号加密密钥 | `APP_ENCRYPTION_PHONE_KEY` |

---

## §12 已知技术债务

以下问题已识别，暂未在 Step 1–17 内修复，列入后续迭代：

| 优先级 | 问题 | 来源 | 建议方案 |
|--------|------|------|---------|
| P2 | Feed 全表扫描（无用户维度个性化） | 架构体检 | Step 18+ 引入 Elasticsearch 或倒排索引 |
| P2 | 搜索用 MySQL FULLTEXT，高并发下性能瓶颈 | 架构体检 | 迁移到 Elasticsearch（Step 18+） |
| P2 | 无 Rate Limit 在 Nginx 层的全局兜底 | 架构限制 | Nginx limit_req_zone |
| P3 | Consumer 无幂等去重（at-least-once 可能重复写） | Step 13 偏离点 | 消息 ID + Redis Set 去重 |
| P3 | RabbitMQ 单点（无镜像队列/仲裁队列） | 当前架构 | 升级为 RabbitMQ 仲裁队列或 Kafka |
| P3 | 日志聚合方案缺失（双实例各自写文件） | Step 15 发现 | ELK / Loki + Promtail |
| P3 | Grafana SLO 告警规则未配置 | Step 16 | Prometheus AlertManager |
| P3 | `super_read_only` 由 init-slave.sh 设置，重启后需重新执行 | Step 14 偏离点 | 写入 slave.cnf 并解决时序问题 |

---

## §13 实际实现 vs 原始 PRD 偏离汇总

| 步骤 | PRD 原始要求 | 实际实现 | 选择原因 |
|------|------------|---------|---------|
| Step 9 | STS 临时凭证直传 | PresignedURL 直传（15min TTL） | 无 per-object STS 权限需求，presign 足够安全 |
| Step 9 | — | post_steps 子表 + image_urls JSON 列 | 轻量，UK 防并发 |
| Step 10 | AuditStatus=0 语义 | 同 PRD（无偏离）| — |
| Step 11 | SHA256(phone \|\| pepper) 二进制拼接 | SHA256(phone + pepper) 字符串拼接 | Go stdlib 等价，字节序一致 |
| Step 12 | 无偏离 | — | — |
| Step 13 | Queue: `cooking.<topic>.<consumer_group>` | Queue: `cooking.<topic>` | EventSubscriber 接口不含 consumer_group 参数 |
| Step 13 | Nack 重试计数后进 DLX | handler error 直接 Nack(requeue=false) | 经典队列无 x-delivery-count，DLX 仍正常工作 |
| Step 14 | slave.cnf 配置 super_read_only | init-slave.sh SET GLOBAL | Docker entrypoint 时序限制 |
| Step 14 | GTID 增量同步 | mysqldump 全量快照起步 | Slave 从完整 schema 开始，避免 DDL 缺失问题 |
| Step 15 | active health check | passive（proxy_next_upstream）| Nginx 开源版不支持 active health check |
| Step 15 | /readiness 路由 | 新增 /health/ready 别名，保留 /readiness | 向后兼容 |
| Step 16 | MySQL 连接池用 GORM callback | sql.DB pull-based Collector | 池级聚合状态，sql.DBStats() 更准确 |
| Step 17 | go test（全量） | services: MySQL+Redis + env-var skip（本地） | CI 方案 B：真实 Docker services，本地 skip |

---

*文档终。后续步骤（18–20 部署上线）以本文档为起点。*
