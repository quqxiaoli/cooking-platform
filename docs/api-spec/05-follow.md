# 05 · 关注模块

> 代码来源：`internal/handler/follow.go`、`internal/service/follow_service.go`、`internal/model/dto/follow.go`。

---

## 路由表

| 动作 | 方法 + URL | 鉴权 |
| --- | --- | --- |
| 关注 | `POST /api/v1/users/:id/follow` | **必须** |
| 取消关注 | `DELETE /api/v1/users/:id/follow` | **必须** |
| 粉丝列表（谁关注了 :id） | `GET /api/v1/users/:id/followers` | 公开 |
| 关注列表（:id 关注了谁） | `GET /api/v1/users/:id/following` | 公开 |

`:id` 永远指**被观察的用户**；关注 / 取关动作的**发起方**是 JWT 里的当前用户。

---

## 1. POST `/api/v1/users/:id/follow` —— 关注

**鉴权**：**必须**

**Path Param**：`id` 被关注用户的 user_id。

**Request Body**：无。

**Response 成功**（`dto.FollowActionResp` @ `dto/follow.go:54`）

```json
{ "code": 0, "data": { "following": true } }
```

**幂等性**：重复关注同一人 → 仍然返回 `following: true`，不报错，不重发事件。

**限制**：

| 规则 | 错误 |
| --- | --- |
| 不能关注自己 | `440101 ErrCannotFollowSelf` |
| 关注上限 3000（`follow.max_following`） | `440102 ErrFollowLimitExceeded` |
| 被关注的人不存在 | `410108 ErrUserNotFound` |

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | :id 非数字或 ≤ 0 |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |
| 410108 | 404 | 目标用户不存在 / 已注销 |
| 440101 | 400 | followerID == targetID |
| 440102 | 400 | 自己关注数已达 3000 |
| 500001 | 500 | DB / Redis 失败 |

**cURL**

```bash
curl -i -X POST https://mellowck.com/api/v1/users/2002/follow \
  -H 'Authorization: Bearer <ACCESS_TOKEN>'
```

---

## 2. DELETE `/api/v1/users/:id/follow` —— 取消关注

**鉴权**：**必须**

**Path Param**：`id` 被取消关注用户的 user_id。

**Response 成功**

```json
{ "code": 0, "data": { "following": false } }
```

**注意**：**取关不幂等**——对当前没有关注的人调 DELETE 会报 `440103 ErrFollowNotFound`（PRD 偏离点，已记录到 99-prd-deltas.md，因为客户端 UI 本身只在"已关注"态显示取消按钮，所以这是显式合约）。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | :id 非法 |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |
| 410108 | 404 | 目标用户不存在 |
| 440103 | 404 | 当前未关注此人 |
| 500001 | 500 | DB / Redis 失败 |

---

## 3. GET `/api/v1/users/:id/followers` —— 粉丝列表

**鉴权**：无（公开）

**Path Param**：`id` 被观察用户。返回的是**关注了 `:id` 的人**。

**Query**（`dto.FollowListQuery` @ `dto/follow.go:40`）

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `cursor` | string | "" | 不透明（内部为 `follows.id`） |
| `size` | int | 20 | `[1, 50]` |

**Response 成功**（`dto.FollowListResp` @ `dto/follow.go:83`）

```json
{
  "code": 0,
  "data": {
    "users": [
      { "id": 3003, "nickname": "厨友1234", "avatar_url": "" },
      { "id": 3004, "nickname": "厨友5678", "avatar_url": "https://..." }
    ],
    "next_cursor": "42",
    "has_more": true
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `users[]` | 每行只有 id / nickname / avatar_url，**不带**关注时间、计数器、是否互关。如需更多字段请单独调 `GET /users/:id` |
| `next_cursor` | `follows.id` 的十进制字符串，**禁止解析**。空字符串 = 末页 |
| 排序 | 按 `follows.id DESC`——**新粉丝在前** |

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | :id / size 非法 |
| 410108 | 404 | 用户不存在 |
| 440104 | 400 | cursor 解析失败（非纯数字） |

---

## 4. GET `/api/v1/users/:id/following` —— 关注列表

**鉴权**：无（公开）

**Path Param**：`id` 被观察用户。返回的是 **`:id` 正在关注的人**。

**Query / Response**：结构与 `/followers` 完全一致（都用 `FollowListQuery` + `FollowListResp`）。排序同样按 `follows.id DESC`，**最近关注的在前**。

---

## 5. 互关状态如何判断？

接口**不返回 `is_mutual` 字段**。前端要展示"互相关注"需要分别打两个判断：

```
GET /users/<我>/following 中是否包含目标
GET /users/<目标>/following 中是否包含我
```

或者只在个人主页用 `GET /users/<目标>/followers` 判断对方是否关注了我。**没有专门的"我是否关注 X"接口**。如果需要，建议前端在用户登录后一次性拉取 `/users/me/following` 全量（上限 3000，size=50 翻 60 页可拿全）并缓存到本地。

---

## 6. 计数器

接口不返回 `follower_count` / `following_count`。这两个字段在 `GET /users/:id`（公开资料）里有，由 CountConsumer 异步维护，**最终一致**（典型滞后 ≤ 10s）。

---

## 7. 不存在的接口

| 路径 | 状态 | 说明 |
| --- | --- | --- |
| `GET /api/v1/users/me/follow-status?target_id=X` | **不存在** | 见 §5 的替代方案 |
| `GET /api/v1/users/:id/mutual` | **不存在** | MVP 不提供互关列表 |
| `POST /api/v1/users/:id/block` | **不存在** | MVP 无拉黑功能 |
