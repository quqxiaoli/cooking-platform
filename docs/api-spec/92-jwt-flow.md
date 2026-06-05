# 92 · JWT 鉴权完整流程

> 代码来源：`pkg/jwt/jwt.go`、`internal/middleware/auth.go`、`internal/service/user_service.go`、`configs/config.yaml:48`、`configs/config.prod.yaml:46`。

---

## 1. 协议要点

| 项 | 值 |
| --- | --- |
| 签名算法 | **HS256（仅 HS256）** |
| 拒绝攻击 | 显式拒绝 `alg: none` |
| Secret | 至少 32 字符（`JWT_SECRET_MIN_32_CHARS`） |
| Claim 字段 | `jti` (UUIDv4，黑名单用) / `sub` (user_id) / `exp` / `iat` |
| 携带方式 | 请求头 `Authorization: Bearer <token>`（**大小写不敏感**，`bearer` / `BEARER` 都接受） |
| TTL（prod） | access **2h**，refresh **168h（7 天）** |
| 黑名单存储 | Redis key `jwt:bl:{jti}`，TTL = access 剩余生命 |

---

## 2. 双 Token 模型

```
                ┌─────────────┐
                │ access_token│  TTL 2h  → 每次业务请求带 Authorization
                ├─────────────┤
                │refresh_token│  TTL 7d  → 仅用于换 access
                └─────────────┘
```

| 区别点 | access_token | refresh_token |
| --- | --- | --- |
| 携带方式 | `Authorization: Bearer <t>` 请求头 | `POST /auth/refresh` 的 JSON body `refresh_token` 字段 |
| 能调任意鉴权接口？ | **能** | **不能**——`refresh_token` 当 Bearer 用会被拒（payload 里有 type 标记） |
| 撤销机制 | jti 进 Redis 黑名单 | **不可撤销**（不入库）——泄露则到期前可一直换 access |
| 续期方式 | 调 `/auth/refresh` 换发新对 | 调 `/auth/refresh` 时同时轮换发新 refresh |

> **MVP 决策**：refresh 不撤销是为了避免一份额外的"refresh 白名单"表 + 每次校验多一次 Redis 命中。代价是 refresh 泄露后无法注销。客户端必须把 refresh **安全保存**（移动端：Keychain / Keystore；Web：HttpOnly Cookie 或后端代理交换）。

---

## 3. 首次登录获取 Token

```
┌────────┐                              ┌────────┐
│ Client │                              │Backend │
└───┬────┘                              └───┬────┘
    │                                       │
    │ 1. POST /auth/send-code               │
    │    {phone}                            │
    ├──────────────────────────────────────▶│
    │                                       │ • Redis SETEX sms:code:<phone>=<code> 300s
    │                                       │ • 调短信网关 (prod) / 写日志 (dev mock)
    │                              200 ok   │
    │◀──────────────────────────────────────┤
    │                                       │
    │ ... 用户输入 6 位验证码 ...             │
    │                                       │
    │ 2. POST /auth/login                   │
    │    {phone, code}                      │
    ├──────────────────────────────────────▶│
    │                                       │ • 校 code
    │                                       │ • 自动注册（如果是新手机）
    │                                       │ • 签 access + refresh
    │                                       │
    │ 200 {access_token, refresh_token,     │
    │      access_token_expires_at, user}   │
    │◀──────────────────────────────────────┤
    │                                       │
    │ 3. 业务请求                            │
    │    Authorization: Bearer <access>     │
    ├──────────────────────────────────────▶│
```

---

## 4. Access Token 过期 → Refresh 流程

**触发条件**：业务请求收到 `HTTP 401 + code 401002 (token expired)`。

```
┌────────┐                              ┌────────┐
│ Client │                              │Backend │
└───┬────┘                              └───┬────┘
    │ 业务请求                              │
    │ Authorization: Bearer <expired>      │
    ├─────────────────────────────────────▶│
    │                                      │
    │ 401 {code: 401002, msg: "token       │ ← 触发 refresh
    │      expired"}                       │
    │◀─────────────────────────────────────┤
    │                                      │
    │ POST /auth/refresh                   │
    │ {refresh_token: <old_refresh>}       │
    ├─────────────────────────────────────▶│
    │                                      │ • 校 refresh 签名 + 过期
    │                                      │ • 校 user 仍存在 + 未封
    │                                      │ • 签新 access + 新 refresh（轮换）
    │                                      │
    │ 200 {access_token: <new_access>,     │
    │      refresh_token: <new_refresh>,   │
    │      access_token_expires_at,        │
    │      token_type: "Bearer"}           │
    │◀─────────────────────────────────────┤
    │                                      │
    │ ① 客户端持久化新 access + 新 refresh  │
    │ ② 丢弃旧 refresh                     │
    │ ③ 用新 access 重放刚才失败的业务请求   │
    │ Authorization: Bearer <new_access>   │
    ├─────────────────────────────────────▶│
    │                                      │
    │ 200 ...                              │
    │◀─────────────────────────────────────┤
```

**关键点**：

- 旧 refresh 在原 TTL 内**仍然有效**（不入黑名单），但建议立即丢弃。
- refresh 同时返回新 access + 新 refresh，**两个都要换**。
- 如果 refresh 也 401（`401002` 过期 / `401003` 无效）→ **彻底登出**，跳登录页。

---

## 5. 登出（黑名单 access）

```
┌────────┐                              ┌────────┐
│ Client │                              │Backend │
└───┬────┘                              └───┬────┘
    │ POST /auth/logout                    │
    │ Authorization: Bearer <access>       │
    ├─────────────────────────────────────▶│
    │                                      │ • 解析 access，取 jti
    │                                      │ • Redis SETEX jwt:bl:<jti>=1 <剩余TTL>
    │                                      │
    │ 200 ok                               │
    │◀─────────────────────────────────────┤
    │                                      │
    │ 此后用这个 access 调任何接口都 401003  │
    │ refresh_token 客户端必须自行清除      │
```

**注意**：

- 登出**只**黑名单 access，refresh **不撤销**（见 §2 决策注释）。
- 客户端务必同时清除本地的 refresh_token——否则下次启动还能 refresh 出新 access。

---

## 6. 客户端实现模板

### 6.1 HTTP 拦截器伪代码

```typescript
async function apiCall(request: Request): Promise<Response> {
  let token = await getStoredAccess();
  let resp = await sendWith(request, token);

  if (resp.status === 401 && resp.body.code === 401002) {
    // access 过期 → 静默 refresh
    try {
      const newPair = await refreshTokens();   // POST /auth/refresh
      await storeAccess(newPair.access_token);
      await storeRefresh(newPair.refresh_token);
      // 重放原请求
      resp = await sendWith(request, newPair.access_token);
    } catch (e) {
      // refresh 也失败 → 彻底登出
      await clearAllTokens();
      navigateToLogin();
      throw e;
    }
  } else if (resp.status === 401 && resp.body.code === 401003) {
    // token 已被黑名单 / 签名非法 → 直接登出
    await clearAllTokens();
    navigateToLogin();
  }
  return resp;
}
```

### 6.2 并发请求 + 同时过期

如果同一时刻 N 个业务请求都拿到 401002，**不能**让 N 个请求都去 refresh——这会产生 N 个新 refresh 把前面的覆盖掉，旧 refresh 虽然不立即失效但都被丢弃了，浪费名额。

**正确做法**：客户端用 Mutex / Promise.race 把 refresh 调用排队，N 个并发请求只触发 **1 次** refresh，剩下 N-1 个 await 那个结果。

```typescript
let refreshPromise: Promise<TokenPair> | null = null;

async function refreshTokens(): Promise<TokenPair> {
  if (refreshPromise) return refreshPromise;
  refreshPromise = doRefresh().finally(() => { refreshPromise = null; });
  return refreshPromise;
}
```

---

## 7. 服务端校验逻辑（仅供前端理解）

来自 `internal/middleware/auth.go`：

```
1. 读 Authorization 头
   • 不存在 → 401001 ErrUnauthorized
2. 拆 "Bearer " 前缀（大小写不敏感）
   • 拆失败 → 401001
3. ParseWithClaims（HS256 校签 + exp 检查）
   • exp < now    → 401002 ErrTokenExpired
   • 签名非法     → 401003 ErrTokenInvalid
   • alg ≠ HS256 → 401003
4. Redis GET jwt:bl:<jti>
   • 命中（在黑名单） → 401003 ErrTokenInvalid
5. 把 user_id 和 jti 存进 gin.Context
```

**前端能从错误码精确知道下一步动作**：

| code | 含义 | 行动 |
| --- | --- | --- |
| 401001 | 缺头 / 头格式错 | 引导登录 |
| **401002** | **过期** | **走 refresh** |
| 401003 | 签名非法 / 黑名单 | 清 token + 引导登录 |

---

## 8. 不存在 / 不支持的功能

| 项 | 说明 |
| --- | --- |
| 多设备登录互踢 | 不支持。同一 user 可签多个 access + refresh 并存 |
| 强制登出某用户全部设备 | 没有接口。要做只能改库（user.is_banned=1，下次任何接口都 410109） |
| OAuth / 第三方登录 | 不支持。MVP 只有手机号 |
| 长 session（"记住我"30 天） | 不支持。固定 access 2h + refresh 7d |
| Session-based 鉴权（Cookie） | 不支持。纯 JWT |
