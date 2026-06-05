# 03 · 点赞模块

> 代码来源：`internal/handler/like.go`、`internal/service/like_service.go`、`internal/model/dto/like.go`。

---

## 模块设计要点

点赞是一种**资源**而不是**动作**——用户与帖子之间的 like 关系要么存在要么不存在。所以走标准 REST：

| 动作 | 方法 + URL |
| --- | --- |
| 点赞 | `POST /api/v1/posts/:id/like` |
| 取消点赞 | `DELETE /api/v1/posts/:id/like` |
| 查询自己是否点过赞 | `GET /api/v1/posts/:id/like` |

三个接口**响应体完全一致**（`dto.LikeResp` @ `dto/like.go:28`），前端用同一段解析逻辑处理。

```json
{ "liked": true, "count": 12 }
```

| 字段 | 含义 |
| --- | --- |
| `liked` | 当前用户**此刻**是否在该帖子的点赞集中（`POST` 成功后恒为 true，`DELETE` 成功后恒为 false） |
| `count` | 该帖**实时**点赞总数。从 Redis (`like:cnt:{post_id}`) 取，**不是** `posts.like_count`（后者由 LikeConsumer 异步刷，滞后 ≤ 3s） |

---

## 1. POST `/api/v1/posts/:id/like` —— 点赞

**鉴权**：**必须**（+ 每用户 200 次 / 24h 频控，命中报 `429001`）

**Path Param**：`id` 帖子主键，正整数。

**Request Body**：无。

**Response 成功**

```json
{
  "code": 0,
  "msg": "ok",
  "data": { "liked": true, "count": 13 },
  "request_id": "..."
}
```

**幂等性**：

- 第 1 次点赞 → `liked: true, count: N+1`，发布 `LikeEvent` 到 MQ。
- 第 2..N 次重复点赞 → `liked: true, count` 不再 +1（不重复入 Redis SADD），**不再发布事件**。
- 客户端可以安全重试。

**允许自赞**：发帖人本人也可以给自己的帖子点赞（service 层无禁止，前端可按产品决定是否隐藏按钮）。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | id 非数字或 ≤ 0 |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |
| 412104 | 404 | 帖子不存在 / is_visible=0 |
| 429001 | 429 | 24h 内已点赞 200 次（不区分对哪个帖） |
| 500001 | 500 | Redis / DB 失败 |

**cURL**

```bash
curl -i -X POST https://mellowck.com/api/v1/posts/1001/like \
  -H 'Authorization: Bearer <ACCESS_TOKEN>'
```

---

## 2. DELETE `/api/v1/posts/:id/like` —— 取消点赞

**鉴权**：**必须**（无频控）

**Path Param**：`id` 帖子主键。

**Response 成功**

```json
{ "code": 0, "data": { "liked": false, "count": 12 } }
```

**幂等性**：未点赞状态下重复 DELETE 也返回 `liked: false`，不发布事件，不报错。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | id 非法 |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |
| 412104 | 404 | 帖子不存在 |
| 500001 | 500 | Redis / DB 失败 |

---

## 3. GET `/api/v1/posts/:id/like` —— 查询点赞状态

**鉴权**：**必须**（这是"我有没有点过赞"的问题，匿名无意义）

**Path Param**：`id` 帖子主键。

**Response 成功**

```json
{ "code": 0, "data": { "liked": false, "count": 12 } }
```

> 公开列表 / 详情接口已自带 `like_count` 字段——只有需要知道"我"的点赞状态时才打这个接口。**不要**在每张 Feed 卡片渲染时挨个调用此接口，请用批量缓存策略：登录后一次性拿用户最近点赞过的 post_id 集合（Redis Key `like:set:user:{uid}`，目前未对外提供专用接口，需要时新加）。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | id 非法 |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |
| 412104 | 404 | 帖子不存在 |

---

## 4. 一致性与时延

| 关注点 | 说明 |
| --- | --- |
| `count` 字段 | 来自 Redis，**实时**（INCR 后立即返回） |
| `posts.like_count`（详情接口里） | 来自 MySQL，由 LikeConsumer 每 3s 或攒满 50 条事件刷一次。**滞后 ≤ 3s** |
| `users.total_likes` | 由 CountConsumer 维护，滞后 ≤ 10s |
| MQ 消息消费失败 | 走 DLX 重试队列（prod RabbitMQ）。dev channel 模式无 DLX，失败仅打日志 |

**前端建议**：

- 用户点赞后立即用 `LikeResp.count` 更新本地 UI，不要刷帖子详情。
- 重新进入 Feed 时也不要立即调 `GET /like` 校对每张卡——`like_count` 字段是 source of truth（虽然滞后，但视觉变化用户感觉不到）。

---

## 5. 不存在的接口

| 路径 | 状态 | 说明 |
| --- | --- | --- |
| `POST /api/v1/posts/:id/unlike` | **不存在** | 用 `DELETE /like` |
| `GET /api/v1/users/me/likes` | **不存在** | MVP 不提供"我点过的赞"列表 |
| `GET /api/v1/posts/:id/likers` | **不存在** | MVP 不提供"谁点了赞"列表（产品取舍：不强化社交压力） |
