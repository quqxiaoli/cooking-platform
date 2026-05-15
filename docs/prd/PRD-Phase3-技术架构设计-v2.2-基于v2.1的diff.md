# 烹饪内容分享平台 · 技术架构设计文档 v2.2

> **文档状态**：已确认
> **所属阶段**：阶段 3 — 技术架构设计
> **作者**：首席 AI 工程师
> **创建日期**：2026-04-15
> **本次更新**：2026-05-11
> **依赖文档**：PRD-Phase1-立项文档-v1.2.md · PRD-Phase2-需求文档-v1.1.md
> **技术栈**：Go · MySQL · Redis · RabbitMQ · Docker · Nginx
> **变更记录**：
> - v1.0 — 初版架构（单实例、无高可用、offset 分页）
> - v2.0 — 全面升级至大厂生产标准：高可用部署、游标分页、OSS 直传、审核状态机、可观测性体系、安全合规、CI/CD、降级预案等
> - v2.1 — 修复 7 项自查问题 + 引入消息队列（RabbitMQ）替代 Redis List/Set 驱动的异步链路 + EventBus 接口抽象（支持 MVP 阶段 Go Channel 实现与生产 MQ 实现无缝切换）
> - **v2.2 — Step 2-5 实现反哺**：吸收第一轮"骨架跑通"过程中沉淀的 28 项实现偏离点，让 PRD 与代码 ground truth 完全对齐。重点变更包括：EventBus 接口签名调整（§4.4）、Consumer 包路径与归属（§3.4 + §2）、用户模块接口扩展（§7.1）、posts 表新增 content 列（§5.2）、Step 4-9 dev mode 临时策略（§9）、Consumer 路由表精简（§3.4）、错误码体系全面升格为 6 位三段式 XYYZZZ（§7.3）。

---

## 目录

1. [整体架构描述](#1-整体架构描述)
2. [Go 项目目录结构](#2-go-项目目录结构)
3. [各层职责与设计原则](#3-各层职责与设计原则)
4. [消息队列设计（EventBus 抽象 + RabbitMQ）](#4-消息队列设计eventbus-抽象--rabbitmq)
5. [数据库设计（MySQL）](#5-数据库设计mysql)
6. [Redis 缓存策略](#6-redis-缓存策略)
7. [RESTful API 设计规范](#7-restful-api-设计规范)
8. [图片上传方案（OSS 直传）](#8-图片上传方案oss-直传)
9. [内容审核流程](#9-内容审核流程)
10. [安全合规设计](#10-安全合规设计)
11. [可观测性体系](#11-可观测性体系)
12. [降级预案与容灾设计](#12-降级预案与容灾设计)
13. [Docker 高可用部署方案](#13-docker-高可用部署方案)
14. [CI/CD 流水线](#14-cicd-流水线)
15. [数据库迁移管理](#15-数据库迁移管理)
16. [关键技术决策记录（ADR）](#16-关键技术决策记录adr)

---

## v2.1 → v2.2 变更摘要

### 偏离点吸收明细（共 28 项 → 26 项进 PRD，2 项被合并）

| 步骤 | 偏离项数 | 进 PRD | 关键变化 |
|---|---|---|---|
| Step 2（EventBus） | 3 | 3 | Handler 签名 / EventBus 接口嵌入 / ConsumerManager 包归属 |
| Step 3（用户模块） | 7 | 7 | refresh/logout 接口 / JWT 黑名单动态 TTL / SMS 三维固定窗口 / ip_hash SHA-256 / phone 分阶段加密 / phone_hash VARCHAR / 默认昵称 |
| Step 4（内容模块） | 7 | 7 | posts.content 列 / dev mode is_visible=1 / PVEvent 时机 / PostEvent 时机 / 作者主页 URL / cook_duration 推迟 / 错误码 412xxx |
| Step 5（点赞模块） | 11 | 9 + 2 合并 | likes 无 deleted_at/UpdatedAt / LikeEvent 双结构双 topic / limit 200/24h / 去 count.q 中转 / 7d 滑动 TTL / 无 MySQL 事务 / 错误码复用 |

> **2 项合并说明**：Step 5 #8（PVEvent 实现时机）与 Step 5 #9（PostEvent 实现时机）的内容与 Step 4 #3、#4 完全等价，并入后避免文档冗余。

### 错误码体系升格（§7.3 全表重写）

PRD v2.1 §7.3 使用 5 位 4xxxx 格式（如 40001 / 40401），实现层从 Step 3 起统一为 6 位三段式 XYYZZZ：

- **X**：HTTP 段位（4/5），与状态码对齐
- **YY**：模块号（00=通用，01=用户，02=内容，03=互动，04=关注，05=搜索...）
- **ZZZ**：模块内序号

本次 v2.2 把 §7.3 全表升格，让 PRD 成为接口契约的唯一真相源。

---

## 1. 整体架构描述

（v2.1 内容沿用，本次未做实质变更，下面只摘核心数据流图）

```
┌─────────────┬───────────────┬──────────────────────────────────┐
│ 用户操作    │ 事件类型       │ Consumer 处理动作                │
├─────────────┼───────────────┼──────────────────────────────────┤
│ 点赞 / 取消  │ event.like     │ 1. LikeConsumer: INSERT/DELETE   │
│             │ event.unlike   │    likes + 增量 UPDATE like_count│
│             │                │ 2. CountConsumer: 维护 users 计数 │
├─────────────┼───────────────┼──────────────────────────────────┤
│ 访问详情页   │ event.pv       │ PVConsumer: 批量增量 UPDATE       │
│             │                │   posts.view_count                │
├─────────────┼───────────────┼──────────────────────────────────┤
│ 内容审核完成 │ event.audit    │ AuditConsumer (Step 10):          │
│             │                │   UPDATE audit_status + is_visible│
│             │                │   写 audit_log + INCR feed:ver    │
├─────────────┼───────────────┼──────────────────────────────────┤
│ 发布内容     │ event.post     │ 1. 提交审核 API（Step 10）        │
│             │                │ 2. INCR feed:ver                  │
│             │                │ 3. CountConsumer: post_count +1   │
└─────────────┴───────────────┴──────────────────────────────────┘
```

> **[v2.2 update from Step 5]** 原 v2.1 设计的 `event.count` 在实现中被废弃 —— CountConsumer 直接订阅 TopicPost / TopicLike / TopicUnlike 三个源 topic 即可维护 users 冗余计数，去掉一道间接事件。详见 §3.4。

---

## 2. Go 项目目录结构

**[v2.2 update from Step 2]** Consumer 相关代码归属变更：v2.1 把 ConsumerManager 放在 `internal/event/` 包；v2.2 拆为独立的 `internal/consumer/` 包，与业务 Consumer 同包，职责更清晰。

```
cooking-platform/
├── cmd/server/main.go                  # 程序入口
│
├── internal/
│   ├── handler/                        # HTTP 路由处理
│   │   ├── user.go     post.go         feed.go      like.go
│   │   ├── search.go   follow.go       upload.go    health.go
│   ├── service/                        # 业务逻辑 + 事件发布
│   ├── repository/                     # 数据访问
│   ├── model/                          # GORM 模型 + DTO 子包
│   ├── middleware/                     # auth / ratelimit / logger / cors / metrics / recovery / request_id
│   ├── cache/                          # Redis 操作封装
│   ├── event/                          # 事件总线（消息传递）
│   │   ├── types.go                    # Topic 常量 + Payload 结构
│   │   ├── bus.go                      # EventPublisher + EventSubscriber 接口
│   │   ├── channel.go                  # MVP 实现
│   │   └── rabbitmq.go                 # 生产实现（Step 13）
│   │
│   └── consumer/                       # [v2.2] Consumer 包独立
│       ├── consumer.go                 # EventConsumer 接口
│       ├── manager.go                  # [v2.2] ConsumerManager 在此（从 event 包迁入）
│       ├── like_consumer.go
│       ├── pv_consumer.go
│       ├── count_consumer.go
│       └── audit_consumer.go           # Step 10 实装
│
├── pkg/                                # 工具包（config / response / errcode / logger / validator / sms / oss / audit / crypto / graceful / metrics / jwt）
├── configs/                            # config.yaml / config.prod.yaml / config.test.yaml
└── migrations/                         # SQL 迁移
```

---

## 3. 各层职责与设计原则

### 3.1-3.3 各层基本职责

（v2.1 内容沿用，无实质变更）

### 3.4 Consumer 路由表

**[v2.2 update from Step 5]** Consumer 与 Topic 的多对多路由表（基于实际实现）：

| Consumer | 订阅 topic | 批量策略 | 主要操作 |
|---|---|---|---|
| `LikeConsumer` | TopicLike + TopicUnlike | 50 条 / 3 秒 | INSERT IGNORE / DELETE likes 表 + 增量 UPDATE posts.like_count；scaleDeltas 比例分配防 RowsAffected≠输入扭曲 |
| `PVConsumer` | TopicPV | 100 条 / 5 秒 | GROUP BY post_id 后增量 UPDATE posts.view_count |
| `CountConsumer` | TopicPost + TopicLike + TopicUnlike | 20 条 / 10 秒 | 三 goroutine 入同一 channel，按 user_id 聚合后一条 UPDATE 同时更新 users.post_count / total_likes；`GREATEST(0, CAST(total_likes AS SIGNED) + ?)` 防 unsigned 下溢 |
| `AuditConsumer` | TopicAudit | Step 10 实装 | UPDATE posts.audit_status + is_visible + INCR feed:ver + 写 audit_log |

**关键设计偏离 v2.1 之处**：

1. **[v2.2 update from Step 5]** ~~`LikeConsumer` 发出 `event.count`~~ → LikeConsumer 不再发 event.count；由 CountConsumer 直接订阅源 topic
2. **[v2.2 update from Step 5]** ~~`CountConsumer` 消费 `count.q` 中转队列~~ → CountConsumer 直接订阅 TopicPost / TopicLike / TopicUnlike 三 topic；Step 13 切 RabbitMQ 时若需要 fan-out 再加 `count.q` 中转
3. **[v2.2 update from Step 5]** `event.count` topic 标记为 **deprecated，never implemented**；types.go 中保留常量但无生产者无消费者，预留给未来其他聚合场景
4. **[v2.2 update from Step 5]** LikeConsumer 不开 MySQL 事务（INSERT/DELETE + UPDATE 分两步执行）。理由：uk_user_post 提供天然幂等 + 增量 SQL 抗 lost-update + Channel 模式无重复投递 → 三重保证已足够。Step 13 切 RabbitMQ 时再评估 transactional outbox

### 3.5 Consumer 实现统一模式

**[v2.2 update from Step 5]** 3 个 Consumer 统一采用 **"订阅 goroutine + 批量 goroutine"双层模式**：

- 订阅 goroutine 只负责 `json.Unmarshal` 和入队（用 `select case <-eventCh: case <-ctx.Done()` 传播 backpressure）
- 批量 goroutine 是单消费者，独占完整批状态（slice / map），避免锁

### 3.6 优雅停机原则

**[v2.2 update from Step 5]** Consumer drain 阶段使用 `context.Background()`（不复用父 ctx）：父 ctx 已取消但 MySQL 写还要完成，否则用户"刚点的赞"在重启边界丢失。`ConsumerManager.wg.Wait()` 兜住总停机时间。

---

## 4. 消息队列设计（EventBus 抽象 + RabbitMQ）

### 4.1-4.2 设计目标与切换路径

（v2.1 内容沿用）

### 4.3 事件类型定义

**[v2.2 update from Step 5]** LikeEvent / UnlikeEvent 拆分为双结构 + 双 Topic：

```go
// internal/event/types.go

type EventType string

const (
    TopicLike   = "event.like"
    TopicUnlike = "event.unlike"
    TopicPV     = "event.pv"
    TopicAudit  = "event.audit"
    TopicPost   = "event.post"
    TopicCount  = "event.count"  // [v2.2] Deprecated, kept for future
)

// 各事件 Payload 定义（实际签名）

type LikeEvent struct {
    UserID   int64 `json:"user_id"`
    PostID   int64 `json:"post_id"`
    AuthorID int64 `json:"author_id"`
    // [v2.2 update from Step 5] 移除 Action 字段，由 topic 本身区分动作
}

type UnlikeEvent struct {
    UserID   int64 `json:"user_id"`
    PostID   int64 `json:"post_id"`
    AuthorID int64 `json:"author_id"`
}

type PVEvent struct {
    PostID   int64 `json:"post_id"`
    ViewerID int64 `json:"viewer_id"`
    IP       string `json:"ip"`
}

type PostEvent struct {
    PostID   int64 `json:"post_id"`
    AuthorID int64 `json:"author_id"`
    SceneTag uint8 `json:"scene_tag"`
}

type AuditEvent struct {
    PostID      int64  `json:"post_id"`
    AuthorID    int64  `json:"author_id"`
    AuditStatus uint8  `json:"audit_status"`
    Remark      string `json:"remark"`
    RawResponse string `json:"raw_response"`
}
```

**为什么拆双结构 + 双 topic**：

1. Consumer 可分别调批量策略（虽然当前 LikeConsumer 仍合用一个 batch loop，但保留弹性）
2. 订阅 goroutine 不需要 `switch action` 分支判断，每个 topic 入口逻辑单一
3. 字段类型可独立演进（未来 like 可能加权重字段，unlike 不需要）

### 4.4 EventBus 接口定义

**[v2.2 update from Step 2]** 接口签名与组合方式调整：

```go
// internal/event/bus.go

// Handler 是 Consumer 注册的回调签名
// [v2.2 update from Step 2] 携带 ctx 便于超时控制和请求追踪；
//   payload 直接给 []byte，Consumer 按需 Unmarshal，
//   切 RabbitMQ 后零修改（MQ 消息体本就是 bytes）
type Handler func(ctx context.Context, payload []byte) error

type EventPublisher interface {
    Publish(ctx context.Context, topic string, payload []byte) error
}

type EventSubscriber interface {
    Subscribe(ctx context.Context, topic string, h Handler) error
}

// [v2.2 update from Step 2] EventBus 用接口嵌入而非 getter 方法
// v2.1 原设计：bus.Publisher().Publish(...)  / bus.Subscriber().Subscribe(...)
// v2.2 实现版：bus.Publish(...)              / bus.Subscribe(...)
type EventBus interface {
    EventPublisher
    EventSubscriber
    Close() error
}
```

**为什么嵌入而非 getter**：

- 业务代码直接 `bus.Publish(...)`，API 更简洁
- 切 RabbitMQ 时实现代码只需提供 `Publish` / `Subscribe` / `Close` 三个方法，不需要额外 `Publisher()` / `Subscriber()` 工厂方法

### 4.5 ChannelBus（MVP 实现）

（v2.1 内容沿用，签名按 §4.4 调整）

### 4.6 RabbitMQ 实现（Step 13）

（v2.1 内容沿用）

---

## 5. 数据库设计（MySQL）

### 5.1 设计原则

（v2.1 内容沿用）

### 5.2 建表 DDL

#### users 表

**[v2.2 update from Step 3]** phone_encrypted 字段语义和 phone_hash 字段类型变化：

```sql
CREATE TABLE `users` (
  `id`               BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `phone_hash`       VARCHAR(64)      NOT NULL  COMMENT '[v2.2: VARCHAR(64) 存 SHA-256 hex，非 BINARY(32)；调试友好优先]',
  `phone_encrypted`  VARCHAR(200)     NOT NULL  COMMENT '[v2.2: Step 3-10 存明文，Step 11 迁移为 AES-GCM 密文（字段长度同时升至 200 兼容密文）]',
  `nickname`         VARCHAR(50)      NOT NULL DEFAULT '',
  `avatar_url`       VARCHAR(500)     NOT NULL DEFAULT '',
  `bio`              VARCHAR(200)     NOT NULL DEFAULT '',
  `status`           TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '0=正常 1=封禁',
  `post_count`       INT UNSIGNED     NOT NULL DEFAULT 0 COMMENT 'CountConsumer 异步同步',
  `total_likes`      INT UNSIGNED     NOT NULL DEFAULT 0 COMMENT 'CountConsumer 异步同步',
  `follower_count`   INT UNSIGNED     NOT NULL DEFAULT 0 COMMENT 'CountConsumer 异步同步（Step 8）',
  `following_count`  INT UNSIGNED     NOT NULL DEFAULT 0 COMMENT 'CountConsumer 异步同步（Step 8）',
  `created_at`       DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`       DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  `deleted_at`       DATETIME(3)      NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_phone_hash` (`phone_hash`),
  KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

**字段决策记录**：

- **phone_hash VARCHAR(64)**：存 SHA-256 hex 字符串（64 字符）。比 BINARY(32) 多占约 32MB（50 万用户估算），换取 `mysql -e "WHERE phone_hash='abc123...'"` 命令行可直接查询的工程便利
- **phone_encrypted 字段长度 VARCHAR(200)**：明文手机号最长 20 字符，AES-GCM 密文 base64 编码约 100 字符，预留 200 兼容两阶段
- **phone_hash 算法**：纯 SHA-256(phone)；Step 11 迁移时引入 deployment-wide pepper（部署级盐）防彩虹表

#### posts 表

**[v2.2 update from Step 4]** 新增 `content TEXT` 列；is_visible 在 Step 4-9 dev mode 期间默认 1：

```sql
CREATE TABLE `posts` (
  `id`             BIGINT UNSIGNED   NOT NULL AUTO_INCREMENT,
  `user_id`        BIGINT UNSIGNED   NOT NULL                COMMENT '作者 user_id（逻辑外键）',
  `title`          VARCHAR(100)      NOT NULL                COMMENT '标题，1-100 字符',
  `content`        TEXT              NULL                    COMMENT '[v2.2 update from Step 4] MVP 文字版正文，最长 5000 字符；Step 9 引入图片步骤后视情况拆 post_steps 子表',
  `scene_tag`      TINYINT UNSIGNED  NOT NULL                COMMENT '1-8 见 PRD-Phase2 §9',
  `cook_duration`  TINYINT UNSIGNED  NOT NULL DEFAULT 0      COMMENT '0=未填 1-4=时长档位；[v2.2: Step 4 CreatePostReq 暂不收，Step P1 接入]',
  `cover_url`      VARCHAR(500)      NOT NULL DEFAULT ''     COMMENT 'OSS 封面 URL（Step 9 填充）',
  `like_count`     INT UNSIGNED      NOT NULL DEFAULT 0      COMMENT 'LikeConsumer 异步同步',
  `view_count`     INT UNSIGNED      NOT NULL DEFAULT 0      COMMENT 'PVConsumer 异步同步',
  `is_visible`     TINYINT UNSIGNED  NOT NULL DEFAULT 0      COMMENT '0=不可见 1=可见；[v2.2: Step 4-9 dev mode 发帖直接置 1；Step 10 AuditConsumer 接入后由审核决定]',
  `audit_status`   TINYINT UNSIGNED  NOT NULL DEFAULT 0      COMMENT '6 态状态机（见 §9）；[v2.2: Step 4-9 始终为 0，Step 10 起进入完整流转]',
  `audit_remark`   VARCHAR(200)      NOT NULL DEFAULT ''     COMMENT '审核备注',
  `created_at`     DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`     DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  `deleted_at`     DATETIME(3)       NULL DEFAULT NULL       COMMENT '软删除时间戳',
  PRIMARY KEY (`id`),
  KEY `idx_user_created` (`user_id`, `created_at` DESC),
  KEY `idx_visible_created` (`is_visible`, `created_at` DESC),
  KEY `idx_scene_visible_created` (`scene_tag`, `is_visible`, `created_at` DESC),
  KEY `idx_audit_status` (`audit_status`, `created_at` DESC),
  FULLTEXT KEY `ft_title` (`title`) WITH PARSER ngram
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

#### likes 表（Step 5）

**[v2.2 update from Step 5]** likes 表无 deleted_at、无 updated_at：

```sql
CREATE TABLE `likes` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `user_id`    BIGINT UNSIGNED NOT NULL,
  `post_id`    BIGINT UNSIGNED NOT NULL,
  `created_at` DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  -- [v2.2 update from Step 5] 无 updated_at：行不可变（要么存在=已点 要么不存在=未点）
  -- [v2.2 update from Step 5] 无 deleted_at：物理删除即可。软删除会破坏 uk_user_post
  --   唯一约束（取消后重点会主键冲突），除非把 deleted_at 也加进唯一索引
  --   (变 uk_user_post_deleted)，会让索引体积/维护成本/查询计划全部劣化
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_post` (`user_id`, `post_id`),
  KEY `idx_post_id` (`post_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

### 5.3 索引设计原则

（v2.1 内容沿用）

---

## 6. Redis 缓存策略

### 6.1-6.2 缓存设计原则

（v2.1 内容沿用）

### 6.3 Redis Key 命名全表

**[v2.2 update from Step 3 + Step 4 + Step 5]** 实现层最终采用的全部 Key 命名：

| 用途 | Key 模板 | TTL | 来源 |
|---|---|---|---|
| 验证码 | `sms:code:{phone_hash}` | 5 min | Step 3 |
| SMS 短窗限流（同手机 60s 间隔） | `sms:window:{phone_hash}` | 60s | Step 3 |
| SMS 日限手机 | `sms:limit:{phone_hash}` | 24h | Step 3 |
| SMS 日限 IP | `sms:ip:{ip_hash}` | 24h | Step 3；ip_hash = SHA-256(ip) 十六进制 |
| JWT 黑名单 | `jwt:bl:{jti}` | **= access token 剩余 ttl（动态）** | [v2.2 from Step 3] |
| 用户封禁 | `user:ban:{user_id}` | 永久 | Step 3 |
| 用户信息缓存 | `user:info:{user_id}` | 30 min | Step 3 |
| 通用滑动窗口（业务发布限流） | `limit:pub:{user_id}` | = window | Step 3 实现，Step 4 首次挂载 |
| Feed 全局版本号 | `feed:ver` | 永久 | Step 4 |
| Feed 缓存 | `feed:s{scene}:v{ver}:c{cursorMs}` | 5 min | Step 4 |
| PV 去重（登录） | `pv:dup:{post_id}:u{user_id}` | 1h | Step 4 |
| PV 去重（游客） | `pv:dup:{post_id}:i{ip_hash}` | 1h | Step 4 |
| **点赞限流** | `limit:like:{user_id}` | 24h（200/24h） | **[v2.2 from Step 5]** |
| **点赞 SET（幂等判定）** | `like:set:{post_id}` | **7d 滑动** | **[v2.2 from Step 5]** |
| **点赞独立计数器** | `like:cnt:{post_id}` | **7d 滑动** | **[v2.2 from Step 5]** |

**关键设计偏离 v2.1 之处**：

1. **JWT 黑名单 TTL**：v2.1 §10 未明确策略；v2.2 明确为 access token 剩余 ttl 动态计算 —— 过期后自然清理，节省 Redis 内存
2. **like:set / like:cnt TTL 7d 滑动**：每次写入续期 EXPIRE，覆盖周期性流量唤醒；冷帖 TTL 自然过期后下次访问从 MySQL 重建
3. **limit:like 200/24h**：v2.1 §6.3 未指定具体值；v2.2 明确 200 次/24h（正常用户日均 < 50，恶意脚本 200 上限触发风控告警）

### 6.4 缓存失效策略

（v2.1 内容沿用）

### 6.5 事件 → Redis 状态同步

（v2.1 内容沿用，PVEvent 与 PostEvent 在 §6.5 流程图标注由 Step 4 埋点 + Step 5 消费）

### 6.6 SMS 限流策略

**[v2.2 update from Step 3]** 实现采用三维**固定窗口**而非 v2.1 §6.6 示例代码的滑动窗口：

| 维度 | Key | 算法 | 含义 |
|---|---|---|---|
| 同手机短窗 | `sms:window:{phone_hash}` | SETNX + EXPIRE 60s | 同一手机两次发送至少间隔 60 秒 |
| 同手机日限 | `sms:limit:{phone_hash}` | INCR + EXPIRE 24h | 同一手机 24h 内最多 5 次 |
| 同 IP 日限 | `sms:ip:{ip_hash}` | INCR + EXPIRE 24h | 同一 IP 24h 内最多 10 次 |

**为什么固定窗口**：

- 三维滑动窗口实现复杂度是固定窗口的 4-5 倍（需要 Sorted Set + 时间戳清理）
- 业务收益接近 0：用户视角不关心"24h 滑动"和"自然日"的差别
- 通用滑动窗口工具仍实现在 `internal/middleware/ratelimit.go`，供发布限流（Step 4）等更精细的业务使用

---

## 7. RESTful API 设计规范

### 7.1 URL 设计

**[v2.2 update from Step 3]** 用户模块新增 refresh + logout：
**[v2.2 update from Step 4]** 内容模块作者主页用 `/users/:id/posts`：

```
基础路径：/api/v1

认证：
  POST   /api/v1/auth/send-code
  POST   /api/v1/auth/login                  # MVP 验证码登录即注册，无独立 /register
  POST   /api/v1/auth/refresh                # [v2.2 from Step 3] 刷新 access token
  POST   /api/v1/auth/logout                 # [v2.2 from Step 3] JWT 黑名单 logout

用户：
  GET    /api/v1/users/me
  PATCH  /api/v1/users/me                    # MVP 仅暴露 PATCH，PUT 留给 Step 9 头像替换
  GET    /api/v1/users/:id
  GET    /api/v1/users/:id/posts             # [v2.2 from Step 4] 作者主页（与 follow 路由风格一致）

内容：
  POST   /api/v1/posts
  GET    /api/v1/posts/:id

Feed（游标分页）：
  GET    /api/v1/feed?scene_tag=1&cursor=<unixMs>&size=20

互动：
  POST   /api/v1/posts/:id/like
  DELETE /api/v1/posts/:id/like
  GET    /api/v1/posts/:id/like              # 查询当前用户点赞状态

搜索（Step 7）：
  GET    /api/v1/search?q=红烧肉&scene_tag=1&cursor=...&size=20

关注（Step 8）：
  POST   /api/v1/users/:id/follow
  DELETE /api/v1/users/:id/follow
  GET    /api/v1/users/:id/followers?cursor=...&size=20
  GET    /api/v1/users/:id/following?cursor=...&size=20

图片上传（Step 9）：
  POST   /api/v1/upload/presign
  POST   /api/v1/upload/callback

健康检查：
  GET    /health                             # 存活探针
  GET    /readiness                          # 就绪探针，检查 MySQL + Redis 连通性

监控（Step 16）：
  GET    /metrics
```

> **前后端 URL 分工说明**：PRD-Phase2 §F-U03 描述的"个人主页 URL `/u/{user_id}`"是**前端路由**；本节列出的 `/users/:id` 与 `/users/:id/posts` 是**后端 API**。前端 `/u/{id}` 页面内部 fetch 两个 API 拼接渲染。

### 7.2 统一响应格式

（v2.1 内容沿用）

```json
{
    "code": 0,
    "msg": "ok",
    "request_id": "req-a1b2c3d4",
    "data": { ... }
}
```

### 7.3 错误码规范

**[v2.2 update from Step 4 #7 + Step 5 #10]** 错误码全表升格为 6 位三段式 XYYZZZ：

- **X**：HTTP 段位（4=客户端错误，5=服务端错误），与 HTTP 状态码对齐
- **YY**：模块号
  - `00` = 通用（所有模块共享）
  - `01` = 用户（Step 3）
  - `02` = 内容（Step 4）
  - `03` = 互动 / 点赞（**预留**；Step 5 完全复用 412104 + 429001，YAGNI）
  - `04` = 关注（Step 8 预留）
  - `05` = 搜索（Step 7 预留）
  - `06` = 上传（Step 9 预留）
  - `07` = 审核（Step 10 预留）
- **ZZZ**：模块内序号

#### 错误码全表

| 错误码 | HTTP | 模块 | 含义 |
|---|---|---|---|
| `0` | 200 | — | 成功 |
| `400001` | 400 | 通用 | 参数校验失败 |
| `401001` | 401 | 通用 | 未登录 / Authorization header 缺失 |
| `401002` | 401 | 通用 | Token 已过期 |
| `401003` | 401 | 通用 | Token 无效 / 已被黑名单 |
| `403001` | 403 | 通用 | 无操作权限 |
| `404001` | 404 | 通用 | 资源不存在（兜底） |
| `429001` | 429 | 通用 | 请求过于频繁（限流） |
| `500001` | 500 | 通用 | 服务器内部错误 |
| `500002` | 500 | 通用 | 数据库错误 |
| `500003` | 500 | 通用 | Redis 错误（已降级） |
| `503001` | 503 | 通用 | 服务不可用 / 依赖故障 |
| `410101` | 400 | 用户 | 手机号格式错误 |
| `410102` | 400 | 用户 | 验证码格式错误 |
| `410103` | 400 | 用户 | 验证码不存在 / 已过期 |
| `410104` | 400 | 用户 | 验证码不匹配 |
| `410105` | 429 | 用户 | SMS 短窗限流（60s 内重复发送） |
| `410106` | 429 | 用户 | SMS 同手机日限（24h 内 5 次） |
| `410107` | 429 | 用户 | SMS 同 IP 日限（24h 内 10 次） |
| `410108` | 404 | 用户 | 用户不存在 |
| `410109` | 403 | 用户 | 用户已封禁 |
| `410110` | 400 | 用户 | 昵称不合法（含空、超长、敏感词） |
| `410111` | 400 | 用户 | 个人简介超长 |
| `410112` | 400 | 用户 | 头像 URL 不合法 |
| `412101` | 400 | 内容 | 标题为空 |
| `412102` | 400 | 内容 | 标题超长 |
| `412103` | 400 | 内容 | scene_tag 不合法（不在 1-8 范围内） |
| `412104` | 404 | 内容 | 帖子不存在 |
| `412105` | 403 | 内容 | 帖子无操作权限（非作者） |
| `412106` | 400 | 内容 | 游标格式不合法 |
| `412107` | 400 | 内容 | 分页 size 不合法 |
| `412108` | 400 | 内容 | 正文超长 |
| `413xxx` | — | 互动 | **预留段位**（Step 5 完全复用通用 + 内容段，未实际使用 13xxxx） |

**段位预留原则**：每个模块预留 1000 个错误码（足够覆盖该模块所有可能的细分错误），相邻模块号不交叠。若某模块超出预留，可启用 `4YY-501xxx` 等扩展段，不影响既有契约。

### 7.4 认证规范

**[v2.2 update from Step 3]** JWT 黑名单 TTL 策略明确：

- JWT Payload：`{uid, jti, iss, exp, nbf, iat}`
- Auth 中间件链路：解析 JWT → 检查 jwt:bl:{jti} 黑名单 → 检查 user:ban:{uid} 封禁 → 注入 ctx
- **黑名单 TTL = access token 剩余时间**（动态计算）：登出立即拒绝，过期后自然清理，最大化 Redis 内存效率

### 7.5 健康检查

（v2.1 内容沿用）

---

## 8. 图片上传方案

（v2.1 内容沿用，Step 9 实装）

---

## 9. 内容审核流程

### 9.1 审核状态机（含 is_visible 联动）

**[v2.2 update from Step 4 #2]** Step 4-9 之间为 **dev mode 临时策略**：

```
Step 4-9 dev mode（无审核 Consumer）：
  发帖直接 audit_status=0, is_visible=1 → 前台立即可见

Step 10 起进入完整状态机：

用户发布 → audit_status=0, is_visible=0
             │
     异步发出 event.audit + 调用阿里云内容安全 API
             │
    ┌────────┼────────┐
    ▼        ▼        ▼
  通过(1)  疑似(2)  拒绝(4)
  is_visible=1       is_visible=0
                     通知作者
             │
        人工审核
        ┌────┴────┐
        ▼         ▼
      通过(3)   拒绝(5)
  is_visible=1  is_visible=0
```

AuditConsumer 在更新 `audit_status` 的同时更新 `is_visible`，保证两个字段一致。

### 9.2 dev mode → 审核全态切换路径

**[v2.2 from Step 4]** Step 10 启用审核 Consumer 时，需要做的事：

1. PostService.Create 改为发布默认 `is_visible=0, audit_status=0`，并发出 event.audit
2. 启动 AuditConsumer，调用阿里云内容安全 API，根据结果更新两字段
3. 既存的"dev mode 直接可见"数据：保持 is_visible=1 不动（已通过实际人工浏览即为审核），audit_status 在 admin 后台补人工补录

---

## 10. 安全合规设计

（v2.1 内容沿用，关键点补充）

**[v2.2 update from Step 3 #5]** 手机号加密**分阶段策略**：

- Step 3-10：phone_encrypted 字段存明文，phone_hash 走 SHA-256(phone) 作为唯一索引
- Step 11：执行迁移：
  1. 引入 deployment-wide pepper（环境变量注入，与 phone_hash 一起做 `SHA-256(phone || pepper)` 防彩虹表）
  2. 批量 UPDATE phone_encrypted 为 AES-GCM 密文（key 走 KMS / 环境变量）
  3. 应用代码 maskPhone() 改为先解密后脱敏
  4. phone_hash 字段保持不变 → 迁移期间索引无需双写

**[v2.2 update from Step 3 #4]** IP 地址在 Redis 中均以 SHA-256 十六进制存储（`ip_hash`），不入明文：

- 符合数据最小化原则
- 即使 Redis 数据泄露，攻击者无法直接获取 IP 列表

---

## 11-15. 其他章节

（v2.1 内容沿用，本次未做实质变更）

---

## 16. 关键技术决策记录（ADR）

### ADR-001 ~ ADR-018

（v2.1 18 条 ADR 沿用）

### ADR-019 [v2.2] EventBus 接口签名采用接口嵌入 + `func(ctx, []byte)`

**决策**：EventBus 接口由"方法返回接口"改为接口嵌入；Handler 签名携带 ctx 且 payload 用 []byte

**理由**：

1. 业务代码调用更简洁（直接 `bus.Publish()`）
2. RabbitMQ 实现切换时，消息本就是 bytes，避免一层无用的 Unmarshal
3. ctx 让 Handler 内部可做超时控制与请求追踪

**取舍**：失去"明确分离 Publisher / Subscriber 角色"的语义，但实测业务代码同时充当 Publisher 和 Subscriber 的场景几乎不存在，分离收益为零。

### ADR-020 [v2.2] ConsumerManager 归属 internal/consumer 而非 event

**决策**：ConsumerManager 与所有 Business Consumer 一同放在 `internal/consumer/` 包

**理由**：

- `event` 包职责：消息传递（接口 + 两种实现 + Topic 常量）
- `consumer` 包职责：Consumer 生命周期（Manager） + 各业务 Consumer 实现
- 包边界清晰：未来如果 EventBus 提供更多协议实现（NATS、Pulsar 等），不会污染 Consumer 包

### ADR-021 [v2.2] SMS 限流三维固定窗口（不是滑动窗口）

**决策**：三维（phone 短窗 / phone 日限 / IP 日限）均用固定窗口

**理由**：

- 滑动窗口对三维实现复杂度过高（4-5 倍），业务收益接近 0
- 用户视角不关心"24h 滑动"和"自然日"的区别
- 通用滑动窗口工具仍保留在 `internal/middleware/ratelimit.go`，供精细业务（如发布限流）使用

### ADR-022 [v2.2] 手机号加密分阶段实施

**决策**：Step 3 存明文 + 第 11 步迁移 AES-GCM；不在 Step 3 一次性引入加密

**理由**：

- 单步引入加密会让"密钥管理 / 密钥轮换 / KMS 集成"成为单步过大负担，淹没用户模块本身的工程重点
- phone_hash 字段独立于 phone_encrypted，迁移期间无需双写索引
- 实际生产部署前（Step 18 部署上线）必经 Step 11 迁移，无任何线上风险

### ADR-023 [v2.2] LikeEvent/UnlikeEvent 双结构 + 双 Topic

**决策**：拆分为 LikeEvent / UnlikeEvent 两个独立 struct + TopicLike / TopicUnlike 两个独立 topic

**理由**：

- Consumer 可分别调批量策略（虽 LikeConsumer 当前合用 batch loop，弹性保留）
- 订阅 goroutine 无需 switch action 分支
- 字段类型可独立演进

### ADR-024 [v2.2] CountConsumer 直接订阅源 topic 而非走 count.q 中转

**决策**：CountConsumer 直接订阅 TopicPost / TopicLike / TopicUnlike，去掉 event.count 中转事件

**理由**：

- 减少一道间接事件，调用链更短
- LikeConsumer 不再需要"消费 event.like + 发出 event.count"的双重职责
- Step 13 切 RabbitMQ 时若需要 fan-out 再加 `count.q` 中转，零回退成本

**残留**：event.count 常量保留在 types.go 中标记 deprecated，预留给未来其他聚合场景。

### ADR-025 [v2.2] 错误码升格 6 位三段式 XYYZZZ

**决策**：从 v2.1 §7.3 的 5 位 4xxxx 升格为 6 位 XYYZZZ

**理由**：

- 5 位模式下，模块边界用 4xx 第二位区分（如 40x 通用 / 41x 客户端 / 42x 业务限流），但段位混乱（40004 内容违规 vs 40401 内容不存在，"内容"两个字出现两次但段位不同）
- 6 位三段式语义清晰：HTTP 段 / 模块号 / 序号正交，新模块扩展无需挤占别人
- 每个模块预留 1000 个码位（YYZ ZZZ），远超 PRD 列出的 8 ~ 12 个码，长期可扩展性强

**取舍**：客户端 SDK 一次性升级，但 Step 18 部署上线前是单一时机；后期无任何兼容负担。
