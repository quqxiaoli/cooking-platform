# PRD Phase 3 · 技术架构设计 v2.3（Step 12 收口补丁）

> **定位**：本文件是 **v2.2 的增量 diff**，不是全量重写。阅读时以 v2.2 为基线，本文件仅记录"v2.2 与代码现实不符"的部分。
>
> **来源**：`docs/progress/` Step 7–12 进度追踪文件的"本步与 PRD 的偏离点"章节，经 Step 12 收口人工核对后并入。
>
> **适用范围**：Step 7–12（第二轮·补齐功能）所有已确认偏离。第三轮（Step 13–17）偏离将在 Step 17 收口时并入 v2.4。

---

## v2.2 → v2.3 变更摘要

### 偏离点吸收明细（共 18 项 → 18 项全部进 PRD，0 项合并）

| 步骤 | 偏离项数 | 进 PRD | 关键变化 |
|---|---|---|---|
| Step 7（搜索模块） | 3 | 3 | offset 分页 / title-only 搜索 / 450xxx 段位 |
| Step 8（关注模块） | 5 | 5 | 无 Redis 缓存 / follows.id 游标 / AC-1 延后 / 非完美覆盖索引 / F-F02 延后 |
| Step 9（图片上传） | 6 | 6 | PresignedURL 替代 STS / DTO 数组下标顺序 / upload:nonce Key / CDN 缓存降级 / nonce-only callback / 无 upload Topic |
| Step 10（内容审核） | 2 | 2 | AuditStatus=0 复用为提交信号 / alibaba-cloud-sdk-go old SDK |
| Step 11（手机号加密） | 2 | 2 | SHA256 字符串拼接 / 迁移脚本更新 hash 值 |
| Step 12（日志脱敏） | 0 | — | 无偏离，严格按 PRD 实现 |

---

## §5.2 建表 DDL

**[v2.3 from Step 9]** 新增 `post_steps` 子表（migration 000005）：

```sql
CREATE TABLE post_steps (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    post_id    BIGINT UNSIGNED NOT NULL,
    step_no    TINYINT UNSIGNED NOT NULL,
    text       VARCHAR(500)    NOT NULL DEFAULT '',
    image_urls JSON            NOT NULL,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_post_step (post_id, step_no),
    KEY idx_post (post_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

> PRD-Phase2 §4 F-C01 "步骤列表 1..30 步，每步图 0..3 张"由本表承载。`image_urls` JSON 列存储 `[]string` URL 数组（最多 3 张），避免另开 `post_step_images` 二级子表。**无外键**（与全库一致，依赖 Service 层维护完整性）。

**[v2.3 from Step 10]** 新增 `audit_logs` 表（migration 000006）：

```sql
CREATE TABLE audit_logs (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    post_id      BIGINT UNSIGNED NOT NULL,
    audit_status TINYINT         NOT NULL,
    provider     VARCHAR(20)     NOT NULL,
    raw_response TEXT            NOT NULL,
    created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    KEY idx_post (post_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

> **只追加不更新**（append-only），行不可变，故无 `updated_at`。`raw_response TEXT` 无 DEFAULT 值（MySQL 8.0 严格模式不允许 TEXT 列有 DEFAULT）。

---

## §5.3 索引设计原则

**[v2.3 from Step 7]** `posts` 表新增全文索引，支持标题搜索：

```sql
ALTER TABLE posts ADD FULLTEXT INDEX ft_title (title);
```

> 搜索范围：仅 title（tags/content 为 P1，MVP 不做）。`MATCH(title) AGAINST(? IN BOOLEAN MODE)` 配合 `AND is_visible=1`，先全文后过滤。

**[v2.3 from Step 8]** `follows` 表索引设计：

```sql
CREATE TABLE follows (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    follower_id BIGINT UNSIGNED NOT NULL,
    followee_id BIGINT UNSIGNED NOT NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_follow (follower_id, followee_id),
    KEY idx_followee (followee_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

> `ListFollowing` 查询走 `uk_follow` 前缀；`ListFollowers` 走 `idx_followee`。两者均非**完美覆盖索引**（仍需回表取 followee_id / follower_id），3000 上限兜底使回表代价可控，暂不优化。

---

## §6.3 Redis Key 命名全表（追加项）

**[v2.3 from Step 9]** 新增上传 nonce Key：

| 用途 | Key 模板 | TTL | 说明 |
|---|---|---|---|
| 上传 nonce（原子消费） | `upload:nonce:{nonce}` | 15 min（= presign_ttl） | VALUE = JSON `{user_id, object_key, public_url, purpose, content_type, size}`；Redis 6.2+ `GETDEL` 原子消费，防止 callback 重放与竞态 |

> 完整 Key 全表见 v2.2 §6.3；本节仅追加 Step 7–12 新增项。

---

## §7.1 URL 设计（无变更）

v2.2 §7.1 已预收录搜索（Step 7）、关注（Step 8）、上传（Step 9）API 路由，无需更新。

---

## §7.3 错误码规范

**[v2.3 from Step 7–12]** v2.2 错误码全表只到 412xxx，Step 7–12 新增 5 个模块段未入表。补全如下：

### Step 7–12 新增错误码

| 错误码 | HTTP | 模块 | 含义 | 引入步骤 |
|---|---|---|---|---|
| `440101` | 400 | 关注 | 不能关注自己 | Step 8 |
| `440102` | 400 | 关注 | 关注数量上限（max 3000） | Step 8 |
| `440103` | 404 | 关注 | 关注关系不存在 | Step 8 |
| `440104` | 400 | 关注 | 关注列表游标格式不合法 | Step 8 |
| `450101` | 400 | 搜索 | 搜索关键词为空 | Step 7 |
| `450102` | 400 | 搜索 | 搜索游标格式不合法 | Step 7 |
| `460101` | 400 | 上传 | 不支持的文件类型 | Step 9 |
| `460102` | 400 | 上传 | 文件超过大小限制 | Step 9 |
| `460103` | 500 | 上传 | 预签名 URL 生成失败（内部错误） | Step 9 |
| `460104` | 400 | 上传 | nonce 无效（不存在 / 已消费 / 跨用户） | Step 9 |
| `460105` | 400 | 上传 | URL 不在 OSS 白名单内 | Step 9 |
| `460106` | 400 | 上传 | 帖子步骤数据不合法 | Step 9 |
| `470101` | 500 | 审核 | 内容安全 API 调用失败（仅日志，不返回 HTTP 调用方） | Step 10 |
| `470102` | 500 | 审核 | 审核结果写入失败（仅日志） | Step 10 |
| `480101` | 500 | 加密 | 手机号加密失败（仅日志） | Step 11 |
| `480102` | 500 | 加密 | 手机号解密失败（仅日志，GetMyProfile 降级为空串） | Step 11 |

### 段位说明补全

v2.2 §7.3 仅说明了 YY=00~07 的模块号映射，实际实现段位为：

| YY | 模块 | 段位 |
|---|---|---|
| 00 | 通用 | 400xxx / 401xxx / 403xxx / 404xxx / 429xxx / 500xxx / 503xxx |
| 10 | 用户 | 410xxx（411xxx 预留扩展） |
| 12 | 内容 | 412xxx（413xxx 预留互动） |
| 40 | 关注 | 440xxx |
| 50 | 搜索 | 450xxx |
| 60 | 上传 | 460xxx |
| 70 | 审核 | 470xxx（HTTP 500，仅内部） |
| 80 | 加密 | 480xxx（HTTP 500，仅内部） |

> **关于 X 前缀与 HTTP 状态的关系**：v2.2 §7.3 中"X=4 对应客户端错误、X=5 对应服务端错误"的说法在 470xxx/480xxx 上不成立——这两段是 server-internal 错误码（HTTP 500），但沿用 4 前缀以保持全项目编码风格一致。**HTTP 实际状态由 `AppError.HTTPStatus` 字段决定**，与 X 前缀无绑定关系。完整错误码总表见 `docs/errcode/错误码总表-v1.md`。

---

## §8 图片上传方案

**[v2.3 from Step 9]** PRD-Phase3 §8 原设计为 STS 临时凭证，Step 9 实装为 **PresignedURL**。

### 实装方案：PresignedURL

```
客户端                    Go 服务端                      阿里云 OSS
  │                          │                              │
  │── POST /upload/presign ──▶│                              │
  │                          │── SignURL(PUT, ttl=15min) ──▶│
  │                          │◀── presigned_put_url ────────│
  │◀── {upload_url, public_url, nonce, expires_at} ─────────│
  │                          │                              │
  │────── PUT upload_url ────────────────────────────────▶  │
  │◀───────────────── 200 OK ───────────────────────────────│
  │                          │                              │
  │── POST /upload/callback ─▶│                              │
  │      body: {nonce}       │── GETDEL upload:nonce:{n} ──▶Redis
  │                          │◀── nonce_record ─────────────│
  │◀── {url: public_url} ────│                              │
```

**与原 STS 方案的差异**：

| 维度 | 原 PRD §8 STS | 实装 PresignedURL |
|---|---|---|
| 客户端 SDK | 需要 OSS SDK（STS 凭证交换） | 纯 HTTP PUT，零 SDK 依赖 |
| 服务端调用 | 每次 presign 需调 STS API | 本地签名，无网络请求 |
| 权限粒度 | 临时账号级别（可访问 bucket 内任意路径） | object 级别（URL 绑定固定 object_key） |
| TTL | STS token（15min~1h） | URL（`oss.presign_ttl`，默认 15min） |
| 安全性 | 中（账号级泄露面更大） | 高（单个 URL 泄露仅影响该 object） |

> STS 保留为 Step 13+ 的可选升级项（大文件分片上传场景下 STS + 分片 API 优于 PresignedURL）。MVP 流量下 PresignedURL 已满足安全需求。

### Callback 设计（nonce-only）

`POST /upload/callback` 只接受 `{nonce}`，不接受任何 client-supplied URL 或 object_key。Server 从 Redis `upload:nonce:{nonce}` 中读取自签发时存储的 `public_url`，作为响应返回。

> 原因：任何 client-supplied URL 都会打开"OSS 白名单绕过 + 跨账户资源挂载"攻击面。Server 是上传元信息的唯一来源。

---

## §9.2 内容审核 AuditEvent 提交信号

**[v2.3 from Step 10]** v2.2 §4.3 描述 AuditEvent 的 `AuditStatus` 字段值为已知审核结果（1/2/3/4/5）。实装中，`AuditStatus=0`（`model.AuditStatusPending`）**被复用为"请审核此帖"的提交信号**：

```
PostService.Create()
  → 发布 AuditEvent{PostID, AuthorID, AuditStatus=0} 到 TopicAudit

AuditConsumer.handleEvent()
  → 检查 AuditStatus == 0（Pending） → 调 Green API
  → 检查 AuditStatus != 0 → 跳过 API 调用（为人工审核 admin 接口预留路径）
```

**为什么复用 Pending 而非新建事件类型**：

1. `model.AuditStatusPending=0` 语义本就是"待审核"，复用语义清晰
2. 避免新建 `PostAuditSubmitEvent` 类型增加 Consumer 订阅路由复杂度
3. 向后兼容：admin 人工审核携带非 0 结果直接入库，Consumer 跳过 API 调用路径

---

## §10 安全合规设计

**[v2.3 from Step 7–12]** 补充第二轮实装的安全措施：

### 日志脱敏（Step 12 实装）

1. **`pkg/crypto/mask.go`**：公开 `MaskPhone(phone) string`（138\*\*\*\*9876 格式）和 `MaskToken(token) string`（短 token 全遮，长 token 前 8 位 + "..."）
2. **`pkg/logger/fields.go`**：`MaskedPhone(phone) zap.Field` / `MaskedToken(token) zap.Field` ——把脱敏嵌入 zap.Field 类型，调用方无需手动 mask
3. **中间件层防御**：`middleware/logger.go` 在记录 `query` 前执行 `sanitizeQuery()`，对 `phone / mobile / code / token / access_key / secret / password` 等参数 value 替换为 `***`

### 安全响应头（Step 12 实装）

所有 HTTP 响应统一注入（`middleware/security.go`，dev/prod 一致）：

```
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
X-XSS-Protection: 1; mode=block
Referrer-Policy: strict-origin-when-cross-origin
```

> CSP（Content-Security-Policy）和 HSTS（Strict-Transport-Security）在 Step 18 配置 HTTPS + 域名后加入，当前环境无 TLS 不适合注入。

### IP 地址保护（v2.2 已并入，此处确认）

所有 Redis Key 中的 IP 以 `ip_hash = SHA-256(ip)` 十六进制存储，不存明文。

### 手机号加密（v2.2 ADR-022 已规划，Step 11 实装，此处补完整状态）

| 字段 | dev 状态 | prod 状态 |
|---|---|---|
| `phone_encrypted` | 明文（key="" 降级） | AES-256-GCM 密文（base64 encoded） |
| `phone_hash` | `SHA256(phone)`（pepper="" 降级） | `SHA256(phone + pepper)` |
| 日志 | `phone_hash` 或 `phone_masked`（138\*\*\*\*9876） | 同左 |

---

## §16 关键技术决策记录（ADR）新增条目

### ADR-026 [v2.3 from Step 7] 搜索使用 offset 分页而非 keyset

**决策**：`GET /search` 返回 `offset` 翻页，不用 cursor/keyset

**理由**：FULLTEXT 按 relevance score 排序，分数不是单调稳定键，无法作为 keyset 游标。强行用时间戳游标会破坏相关性排序。

**取舍**：offset 在深翻页时有性能退化（`LIMIT 100 OFFSET 10000`），但搜索用户极少翻到第 N 页，且 MySQL FULLTEXT 全表扫描本就不适合超大 offset——搜索深翻页场景的正解是 Elasticsearch，MVP 阶段可接受。

---

### ADR-027 [v2.3 from Step 7] 搜索范围 title-only（tags 为 P1）

**决策**：FULLTEXT 索引仅建在 `posts.title` 列，不覆盖 `content` 和 `scene_tag`

**理由**：tags 搜索需要 JSON 展开或独立 tag 表，设计成本不低；PRD §7 F-S01 "按标题/标签搜索" 中标签部分明确标注为 P1。content 全文搜索在 MySQL FULLTEXT 下效果差，P1 阶段应切 Elasticsearch。

---

### ADR-028 [v2.3 from Step 8] 关注数据只写 MySQL，不引入 Redis Set 缓存

**决策**：关注/取消关注直接 INSERT/DELETE `follows` 表，不维护 Redis follow set

**理由**：

1. 关注列表读取非热路径（非 Feed 流关键路径）
2. 3000 上限兜底使 MySQL 单次 B-tree 扫描代价可控
3. 双写 Redis + MySQL 引入一致性难题（Redis 写成功、MySQL 失败的回滚），MVP 不值得

**取舍**：关注状态查询（"我是否关注了 A"）需要走 MySQL `WHERE follower_id=? AND followee_id=?`，有 `uk_follow` 唯一索引支持，O(log n)，可接受。

---

### ADR-029 [v2.3 from Step 8] 关注列表游标走 follows.id 而非 created_at

**决策**：`ListFollowers` / `ListFollowing` keyset 游标字段为 `follows.id`

**理由**：`id` 是单调递增主键，并发写入时无排序不确定性；`created_at` 精度为 3ms，并发注册用户在同一毫秒关注同一人会导致游标歧义。

---

### ADR-030 [v2.3 from Step 9] PresignedURL 替代 STS 临时凭证

**决策**：OSS 直传用 PresignedURL（`Bucket.SignURL`），不用 STS

**理由**：见 §8 对比表。核心点：object 级权限 + 本地签名（零网络请求）+ 客户端零 SDK 依赖，安全性高于账号级 STS 凭证。

**适用边界**：大文件分片上传（>100MB）场景下 STS + 分片 API 更优，Step 13+ 可补充实现。

---

### ADR-031 [v2.3 from Step 9] CallbackReq nonce-only 设计

**决策**：`POST /upload/callback` 只接受 `{nonce}`，server 从 Redis 读取 `public_url`

**理由**：Server 是上传元信息的唯一权威来源。接受 client-supplied URL 会引入：①OSS 白名单绕过（客户端伪造非白名单 URL）；②跨账户挂载（用 A 的 nonce 配 B 的 URL）。

---

### ADR-032 [v2.3 from Step 10] AuditStatus=0 复用为审核提交信号

**决策**：`AuditEvent.AuditStatus=0` 作为"请 AuditConsumer 调 API 审核此帖"的信号

**理由**：避免新建 `PostAuditSubmitEvent` 类型；`model.AuditStatusPending=0` 本就表示"未审核"，语义自洽；向后兼容 admin 人工审核路径。

---

### ADR-033 [v2.3 from Step 11] SHA256(phone + pepper) 字符串拼接

**决策**：`phone_hash = hex(SHA256([]byte(phone + pepper)))` 使用字符串拼接而非二进制拼接

**理由**：Go `crypto/sha256` 接受 `[]byte`，`[]byte(phone + pepper)` 与"二进制拼接"在 pepper 无 NUL 字节时完全等价。无长度扩展攻击风险（SHA-256 不受长度扩展攻击影响）。

---

## 未受影响的章节

以下 v2.2 章节在 Step 7–12 实装中无偏离，保持不变：

- §1 整体架构描述
- §2 Go 项目目录结构（Step 9 新增 `pkg/oss/`、Step 10 新增 `pkg/audit/`，追加到目录树但未改变结构原则）
- §3 各层职责与设计原则
- §4 消息队列设计（EventBus 抽象 + RabbitMQ）
- §6.1/6.2/6.4/6.5/6.6 缓存设计（除 §6.3 追加 upload:nonce Key）
- §7.2 统一响应格式
- §7.4 认证规范
- §7.5 健康检查
- §11–15 其他章节
