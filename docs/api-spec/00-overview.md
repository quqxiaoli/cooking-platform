# 00 · API 总览

> 本系列文档基于 **真实代码**（截止 2026-05-23）反推，不再以 docs/prd/ 为准。
> 任何与 PRD 的差异已记录到 [99-prd-deltas.md](./99-prd-deltas.md)。

---

## 1. Base URL

| 环境 | URL |
| --- | --- |
| 生产 | `https://mellowck.com` |
| 本地开发 | `http://127.0.0.1:8080` |

所有业务接口统一前缀 `/api/v1/`；基础设施探针（`/health` / `/readiness` / `/metrics`）不带 `/api` 前缀。
代码依据：`cmd/server/main.go:402` `v1 := r.Group("/api/v1")`。

---

## 2. 统一响应格式

所有接口（成功 / 失败）共享同一 JSON 信封，定义在 `pkg/response/response.go:16`：

```json
{
  "code": 0,
  "msg": "ok",
  "data": { },
  "request_id": "uuid-v4"
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `code` | int | 业务码。`0` = 成功，其它见 [90-errcodes.md](./90-errcodes.md) |
| `msg` | string | 人读消息，固定英文（如 `"ok"`、`"invalid request parameters"`），不做 i18n |
| `data` | object / array / null | 业务负载。失败时通常省略；零值时省略（`omitempty`） |
| `request_id` | string | 全链路追踪 ID，从请求头 `X-Request-ID` 透传，缺失则后端生成 UUIDv4，并写回响应头 |

**HTTP 状态码与 `code` 的关系**：
- HTTP 200 == `code: 0` 业务成功（`pkg/response/response.go:24` Success）。
- HTTP 4xx / 5xx 表示业务失败，`code` 是更细的业务码（见 `pkg/errcode/errcode.go`）。
- 唯一例外：HTTP 503（`/readiness` 健康检查失败），`code` 为 `503001`，`data` 含子项状态。

**code 字段示例**：

| 场景 | HTTP | code | msg |
| --- | --- | --- | --- |
| 业务成功 | 200 | 0 | `ok` |
| 参数错误 | 400 | 400001 | `invalid request parameters` |
| 未鉴权 | 401 | 401001 | `unauthorized` |
| Token 过期 | 401 | 401002 | `token expired` |
| Token 无效 / 在黑名单 | 401 | 401003 | `token invalid` |
| 频控 | 429 | 429001 | `too many requests` |
| 服务器错误 | 500 | 500001 | `internal server error` |
| 健康检查失败 | 503 | 503001 | `service unavailable` |

---

## 3. 鉴权

### 3.1 协议

- **方案**：JWT Bearer Token（HS256 签名）。
- **请求头**：`Authorization: Bearer <access_token>`。
- 中间件位置：`internal/middleware/auth.go:45` `Auth(v TokenVerifier)`。
- 大小写宽松：`bearer` 也接受（`auth.go:108`）。

### 3.2 双 Token 模型

| Token | 默认 TTL | 用途 | 携带方式 |
| --- | --- | --- | --- |
| `access_token` | `2h`（`configs/config.yaml:48`） | 每次业务调用 | `Authorization: Bearer <token>` 请求头 |
| `refresh_token` | `168h`（7 天） | 仅用于换发新的 access | `POST /api/v1/auth/refresh` 的 JSON body `refresh_token` 字段 |

> Refresh token **不能** 当 Bearer 使用 —— 它只授权"换新 access"这一种动作。详见 [92-jwt-flow.md](./92-jwt-flow.md)。

### 3.3 Token 下发

- `POST /api/v1/auth/login` 成功时一次性下发 access + refresh 对（`internal/service/user_service.go:204` `issueTokenPair`）。
- `POST /api/v1/auth/refresh` 验证旧 refresh 后，**同时** 下发新的 access + refresh（**轮换**，旧 refresh 在原 TTL 内仍然有效，但建议丢弃）。

### 3.4 Token 撤销

- `POST /api/v1/auth/logout` 把当前 access 的 `jti` 写入 Redis 黑名单 `jwt:bl:{jti}`，TTL = 剩余有效期（`user_service.go:244` `Logout`）。
- Refresh token 本身 **不入库**、**不可撤销**，泄露则到期前可一直换新 access —— 客户端必须妥善保管。

### 3.5 路由鉴权矩阵（来自 `cmd/server/main.go:402-509`）

| 路径 | 方法 | 鉴权 |
| --- | --- | --- |
| `/api/v1/auth/send-code` | POST | 公开 |
| `/api/v1/auth/login` | POST | 公开 |
| `/api/v1/auth/refresh` | POST | 公开 |
| `/api/v1/auth/logout` | POST | **必须** |
| `/api/v1/users/:id` | GET | 公开 |
| `/api/v1/users/:id/posts` | GET | 公开 |
| `/api/v1/users/:id/followers` | GET | 公开 |
| `/api/v1/users/:id/following` | GET | 公开 |
| `/api/v1/users/:id/follow` | POST / DELETE | **必须** |
| `/api/v1/users/me` | GET / PATCH | **必须** |
| `/api/v1/posts` | POST | **必须**（+ 频控） |
| `/api/v1/posts/:id` | GET | 公开 |
| `/api/v1/posts/:id/like` | POST | **必须**（+ 频控） |
| `/api/v1/posts/:id/like` | DELETE / GET | **必须** |
| `/api/v1/feed` | GET | 公开 |
| `/api/v1/search` | GET | 公开（+ 每 IP 频控） |
| `/api/v1/upload/presign` | POST | **必须**（+ 频控） |
| `/api/v1/upload/callback` | POST | **必须** |
| `/health` / `/readiness` / `/health/ready` | GET | 公开 |
| `/metrics` | GET | 公开但 nginx 不外暴露 |

---

## 4. 分页约定

所有列表接口（feed / search / followers / following / 作者帖子）采用 **游标分页**：

```
GET /api/v1/feed?cursor=<opaque>&size=20
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `cursor` | query string | **不透明** 字符串，首页传 `""`（或不传），后续页传上一次响应的 `next_cursor` |
| `size` | query int | 取值 `[1, 50]`，默认 `20`。超界报 412107 `ErrPageSizeInvalid` |

**响应统一字段**（`internal/model/dto/post.go:157` `FeedResp`）：

```json
{
  "posts": [ /* 列表项数组，列表为空时为 [] 而非 null */ ],
  "next_cursor": "1714200000000",
  "has_more": true
}
```

- `next_cursor` 为 `""` 时表示 **最后一页**。
- `has_more` 与 `next_cursor != ""` 等价 —— 客户端任选一个判断（DTO 注释建议同时使用，但二者必然同步）。
- **客户端禁止解析 cursor 的内容**。内部今天的编码（仅供参考、随时可能改）：
  - Feed / 作者帖子：`created_at` 的 UnixMilli 十进制字符串（`internal/service/post_service.go:459`）
  - Search：偏移量 OFFSET 的十进制字符串（`internal/service/search_service.go:209`）
  - Follow 列表：`follows.id` 的十进制字符串（`internal/service/follow_service.go:374`）

- 关注列表 size 上限由 `configs/config.yaml:158` `follow.max_list_size` 配置，当前 `50`。

---

## 5. 时间格式

**统一使用 `int64` 的 UnixMilli 毫秒时间戳**，不使用 ISO 8601 字符串。
依据：`internal/model/dto/post.go:14-17` 注释明确选定毫秒整数。

涉及字段：
- `created_at`、`updated_at`、`access_token_expires_at`、`expires_at`（OSS 直传过期）等。

客户端转本地时间：`new Date(ms)`。

---

## 6. Content-Type 约定

- **请求**：所有 POST / PATCH 请求体 **必须** 使用 `application/json; charset=utf-8`（gin 默认 `ShouldBindJSON`）。
- **响应**：固定 `application/json; charset=utf-8`。
- 上传图片是 **客户端直传 OSS**，由后端签发 URL；详见 [91-oss-upload.md](./91-oss-upload.md)。

---

## 7. CORS 实际配置

中间件 `internal/middleware/cors.go:32`，配置从 yaml 读取（`configs/config.*.yaml` 的 `cors` 节）。

| 环境 | allowed_origins | allowed_methods | allowed_headers | expose_headers |
| --- | --- | --- | --- | --- |
| 开发（`config.yaml:165-168`） | `["*"]` | GET, POST, PUT, PATCH, DELETE, OPTIONS | Origin, Content-Type, Authorization, X-Request-ID | X-Request-ID |
| 生产（`config.prod.yaml:155-161`） | `["https://cooking.example.com"]`（go-live 时替换为 `https://mellowck.com`） | 同上 | 同上 | 同上 |

> 生产 `allowed_origins = ["*"]` 会被 `config.Validate` 拒绝启动（CLAUDE.md §4 / Step 18 红线）。

- OPTIONS 预检请求短路返回 `204 No Content`（`cors.go:71`）。
- 非白名单 Origin → **不写** `Access-Control-Allow-Origin`，浏览器自动拦截。
- 通配 `*` 直接返回 `*`；显式列表则 echo Origin 并附 `Vary: Origin`。

---

## 8. 安全响应头（来自 `internal/middleware/security.go`）

所有响应都会带上：

| Header | 值 |
| --- | --- |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `X-XSS-Protection` | `1; mode=block` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |

生产 nginx 另外注入 `Strict-Transport-Security: max-age=31536000; includeSubDomains`（HSTS 1 年）。

---

## 9. 请求 ID 透传

`internal/middleware/request_id.go:16`：
- 客户端可发 `X-Request-ID` 头自定义（建议 UUIDv4）。
- 缺失则后端生成。
- 响应一律带 `X-Request-ID` 响应头；同时写入响应体的 `request_id` 字段。
- 排障时把这个值给后端，可直接定位日志。

---

## 10. 频控规则（来自 `cmd/server/main.go:436-507` + `service/user_service.go`）

| 端点 | 维度 | 上限 | 窗口 | 命中错误 |
| --- | --- | --- | --- | --- |
| POST /auth/send-code | 同一 phone window | 1 | 60s | 410105 |
| POST /auth/send-code | 同一 phone 日 | 5 | 24h | 410106 |
| POST /auth/send-code | 同一 IP 日 | 10 | 24h | 410107 |
| POST /posts | 每用户 | 20 | 24h | 429001 |
| POST /posts/:id/like | 每用户 | 200 | 24h | 429001 |
| GET /search | 每 IP | 30 | 60s | 429001 |
| POST /upload/presign | 每用户 | 30 | 1h | 429001 |

中间件 `internal/middleware/ratelimit.go:91` —— Redis sorted-set 滑动窗口算法。

> nginx 还有一层 per-IP 限流，前端被 429 时无法区分是后端业务还是网关层；只要看到 429 就按"放慢节奏"处理。
