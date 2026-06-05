# 01 · 用户模块（含鉴权）

> 代码来源：`internal/handler/user.go`、`internal/handler/follow.go`、`internal/service/user_service.go`、`internal/model/dto/user.go`。

---

## 1. POST `/api/v1/auth/send-code` —— 发送验证码

**鉴权**：无

**Request Body**（`dto.SendCodeReq` @ `dto/user.go:16`）

| 字段 | 类型 | 必填 | 校验 |
| --- | --- | --- | --- |
| `phone` | string | 是 | `binding:"required,phone"`（中国大陆 11 位手机号，自定义校验器在 `pkg/validator`） |

**Response 成功**（`dto.SendCodeResp`）

```json
{
  "code": 0,
  "msg": "ok",
  "data": { "expires_in": 300 },
  "request_id": "..."
}
```

| 字段 | 含义 |
| --- | --- |
| `expires_in` | 验证码有效时间秒数（默认 `5m`，`config.yaml:68`） |

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | JSON 体解析失败 |
| 410101 | 400 | phone 格式不符合 11 位规则 |
| 410105 | 429 | 同一手机 60s 内重复请求 |
| 410106 | 429 | 同一手机当天 ≥ 5 次 |
| 410107 | 429 | 同一 IP 当天 ≥ 10 次 |
| 500001 | 500 | Redis 或短信网关失败 |

**cURL**

```bash
curl -i -X POST https://mellowck.com/api/v1/auth/send-code \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800138000"}'
```

**重要**：dev 环境 `sms.provider=mock`，验证码不真发；从 app 容器日志里捞：
```
docker exec cooking-app1-prod tail -f /var/log/cooking/app.log | grep '"phone":"13800138000"'
```

---

## 2. POST `/api/v1/auth/login` —— 登录 / 自动注册

**鉴权**：无

**Request Body**（`dto.LoginReq` @ `dto/user.go:30`）

| 字段 | 类型 | 必填 | 校验 |
| --- | --- | --- | --- |
| `phone` | string | 是 | `required,phone` |
| `code` | string | 是 | `required,len=6,numeric`（6 位纯数字） |

**Response 成功**（`dto.LoginResp` —— TokenPair + 用户公开资料）

```json
{
  "code": 0,
  "msg": "ok",
  "data": {
    "access_token": "eyJhbGc...",
    "refresh_token": "eyJhbGc...",
    "access_token_expires_at": 1714207200000,
    "token_type": "Bearer",
    "user": {
      "id": 1001,
      "nickname": "厨友8000",
      "avatar_url": "",
      "bio": "",
      "post_count": 0,
      "total_likes": 0,
      "follower_count": 0,
      "following_count": 0,
      "created_at": 1714200000000
    }
  },
  "request_id": "..."
}
```

> **首次登录自动注册**（`user_service.go:171`），昵称默认 `"厨友" + phone 末 4 位`（`user_service.go:422`）。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | 字段缺失 / 格式错 |
| 410103 | 400 | 验证码不存在（未发 / 已过期 / 已用过） |
| 410104 | 400 | 验证码不匹配 |
| 410109 | 403 | 账户被封禁 |
| 480101 | 500 | 注册时手机加密失败 |
| 480103 | 500 | `encryption.phone_key` 未配置 |

**cURL**

```bash
curl -i -X POST https://mellowck.com/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800138000","code":"123456"}'
```

---

## 3. POST `/api/v1/auth/refresh` —— 刷新 Token

**鉴权**：无（但需有效 refresh_token）

**Request Body**（`dto.RefreshReq` @ `dto/user.go:40`）

| 字段 | 类型 | 必填 |
| --- | --- | --- |
| `refresh_token` | string | 是 |

**Response 成功**（`dto.TokenPair`）

```json
{
  "code": 0,
  "data": {
    "access_token": "eyJhbGc...",
    "refresh_token": "eyJhbGc...",
    "access_token_expires_at": 1714207200000,
    "token_type": "Bearer"
  }
}
```

> 同时返回新的 access + refresh（**轮换**）。客户端建议丢弃旧 refresh。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | refresh_token 缺失 |
| 401002 | 401 | refresh_token 过期 |
| 401003 | 401 | refresh_token 签名 / 内容非法 |
| 410108 | 404 | uid 对应的用户不存在（被删） |
| 410109 | 403 | 账户被封 |

---

## 4. POST `/api/v1/auth/logout` —— 登出

**鉴权**：**必须**（请求头 `Authorization: Bearer <access_token>`）

**Request Body**：无

**Response 成功**

```json
{ "code": 0, "msg": "ok", "data": null, "request_id": "..." }
```

实现细节（`user_service.go:244`）：
- 解析 Authorization 头的 access_token，取出 `jti`。
- 把 `jti` 写入 Redis 黑名单 key `jwt:bl:{jti}`，TTL = access token 剩余生命。
- **不撤销 refresh_token** —— 客户端需自行丢弃。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 401001 | 401 | Authorization 头缺失 / 格式错 |
| 401002 | 401 | access_token 已过期 |
| 401003 | 401 | access_token 无效或已在黑名单 |

---

## 5. GET `/api/v1/users/:id` —— 公开资料

**鉴权**：可选（`OptionalAuth`，匿名可用）。带有效 `Authorization: Bearer <access>` 时响应里的 `is_following` 反映"我是否已关注此用户"；不带或 token 无效时该字段固定 `false`。

**Path Param**：`id` 必须是正整数 user_id。

**Response 成功**（`dto.UserPublicResp` @ `dto/user.go:69`）

```json
{
  "code": 0,
  "data": {
    "id": 1001,
    "nickname": "厨友8000",
    "avatar_url": "https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/avatar/1001/202605/xxx.jpg",
    "bio": "爱做菜的程序员",
    "post_count": 15,
    "total_likes": 234,
    "follower_count": 12,
    "following_count": 33,
    "created_at": 1714200000000,
    "is_following": true
  }
}
```

| 字段 | 说明 |
| --- | --- |
| `is_following` | 当前 access token 标识的用户是否已关注此用户。匿名 / 自己看自己 → 固定 `false`。基于 `follows` 表 `uk_follower_following` 单次索引命中，O(1)。token 过期 / 黑名单 → 当作匿名（不报 401）|

> 计数字段（post_count / total_likes / follower_count / following_count）由 CountConsumer 异步维护，**最终一致**（典型滞后 ≤ 10s）。`is_following` 走 `follows` 源表，**强一致**（写完读得到）。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | id 非数字或 ≤ 0 |
| 410108 | 404 | 用户不存在或已注销 |

---

## 6. GET `/api/v1/users/me` —— 我的资料（含手机号脱敏）

**鉴权**：**必须**

**Response 成功**（`dto.UserPrivateResp` = `UserPublicResp` + `phone_masked`）

```json
{
  "code": 0,
  "data": {
    "id": 1001,
    "nickname": "厨友8000",
    "avatar_url": "",
    "bio": "",
    "post_count": 0,
    "total_likes": 0,
    "follower_count": 0,
    "following_count": 0,
    "created_at": 1714200000000,
    "phone_masked": "138****8000"
  }
}
```

`phone_masked`：手机号中间四位用 `*` 替换（`pkg/crypto/phone.go` `MaskPhone`）。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |
| 410108 | 404 | 当前用户已被删除（异常） |

---

## 7. PATCH `/api/v1/users/me` —— 更新资料（部分更新）

**鉴权**：**必须**

**Request Body**（`dto.UpdateProfileReq` @ `dto/user.go:101`）—— 所有字段为可选指针，**`null` = 不动，`""` = 清空**：

| 字段 | 类型 | 校验 |
| --- | --- | --- |
| `nickname` | string? | `omitempty,min=1,max=50`。service 层会 trim 空白，trim 后为空报 410110 |
| `avatar_url` | string? | `omitempty,max=500`。service 层用 OSS 白名单校验前缀必须等于 `oss.url_prefix` |
| `bio` | string? | `omitempty,max=200`。允许空字符串 |

**Response 成功**

```json
{ "code": 0, "msg": "ok", "data": null }
```

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | JSON 体解析或基础校验失败 |
| 410110 | 400 | nickname trim 后为空 |
| 460105 | 400 | avatar_url 不在 OSS 白名单（`url_prefix` 前缀不符） |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |

**cURL**

```bash
curl -i -X PATCH https://mellowck.com/api/v1/users/me \
  -H 'Authorization: Bearer <ACCESS_TOKEN>' \
  -H 'Content-Type: application/json' \
  -d '{"nickname":"新昵称","bio":"做饭爱好者"}'
```

清空头像：

```bash
curl -i -X PATCH https://mellowck.com/api/v1/users/me \
  -H 'Authorization: Bearer <ACCESS_TOKEN>' \
  -H 'Content-Type: application/json' \
  -d '{"avatar_url":""}'
```

---

## 8. GET `/api/v1/users/:id/posts` —— 作者帖子列表

**鉴权**：可选（`OptionalAuth`）。带有效 token 时每个 `posts[].liked_by_me` 反映当前用户是否已点赞；匿名 / 无效 token → 全部 `false`。

**Path Param**：`id` 作者 user_id。

**Query**：

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `cursor` | string | "" | 不透明分页游标 |
| `size` | int | 20 | `[1, 50]` |

**Response 成功**（`dto.FeedResp` —— 与 feed 共享结构，详见 [02-post.md](./02-post.md)）

```json
{
  "code": 0,
  "data": {
    "posts": [ { "id": 1, "title": "...", "scene_tag": 1, "scene_name": "出租屋", "cover_url": "", "like_count": 0, "view_count": 0, "author": { "id": 1001, "nickname": "...", "avatar_url": "" }, "created_at": 1714200000000, "liked_by_me": false } ],
    "next_cursor": "1714199000000",
    "has_more": true
  }
}
```

只返回 `is_visible=1` 的帖子（`internal/repository/post_repository.go` 的 `ListByUser`）。`liked_by_me` 实现见 [02-post.md §3](./02-post.md)。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | id 非数字 |
| 412107 | 400 | size 超界 |

---

## 9. 不存在的接口（前端可能误以为有）

| 路径 | 状态 | 说明 |
| --- | --- | --- |
| `POST /api/v1/auth/register` | **不存在** | 首次登录自动注册，没有独立注册接口 |
| `POST /api/v1/auth/send-sms`、`/sms/send` | **不存在** | 路径是 `/auth/send-code` |
| `GET /api/v1/users/me/phone`（明文手机号） | **不存在** | 即使是本人，只能拿到 `phone_masked` |
| `DELETE /api/v1/users/me` | **不存在** | MVP 没有注销账户接口 |
