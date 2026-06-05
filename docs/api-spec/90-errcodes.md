# 90 · 完整错误码表

> 代码来源：`pkg/errcode/errcode.go`。
>
> 这张表是错误码的**唯一权威**。任何接口文档里提到的错误码都必须在这里能查到；新错误码必须先在 `errcode.go` 末尾追加再写到本表（CLAUDE.md §4.4）。
>
> **不要根据 `code` 数字推 HTTP 状态码**——`code` 的 X 段位历史上不与 HTTP 状态严格对应（详见 `errcode.go` 包注释）。两者关系由 `AppError.HTTPStatus` 字段决定，**HTTPStatus 是唯一真相源**。

---

## 1. 段位规划

| 段位 | 模块 |
| --- | --- |
| 400xxx / 401xxx / 403xxx / 404xxx / 429xxx | 通用 |
| 410xxx | 用户 / 鉴权 |
| 412xxx | 内容（帖子） |
| 440xxx | 关注 |
| 450xxx | 搜索 |
| 460xxx | 上传 |
| 470xxx | 审核（**仅内部日志**，永不出现在 HTTP 响应） |
| 480xxx | 加密（**仅内部日志**） |
| 500xxx | 通用 5xx |
| 503xxx | 服务不可用 |

---

## 2. 通用错误码

| code | HTTP | 名称 | msg | 前端处理建议 |
| --- | --- | --- | --- | --- |
| 0 | 200 | Success | `ok` | 业务成功 |
| 400001 | 400 | ErrInvalidParams | `invalid request parameters` | 检查请求格式 / 字段类型 |
| 401001 | 401 | ErrUnauthorized | `unauthorized` | Authorization 头缺失 / 格式错。引导用户登录 |
| **401002** | **401** | **ErrTokenExpired** | **`token expired`** | **触发 refresh token 流程**（见 [92-jwt-flow.md](./92-jwt-flow.md)） |
| 401003 | 401 | ErrTokenInvalid | `token invalid` | token 签名 / 内容非法 / 已登出。**清本地 token，引导重新登录** |
| 403001 | 403 | ErrForbidden | `forbidden` | 当前账号无权限。无重试价值 |
| 404001 | 404 | ErrNotFound | `resource not found` | 通用 404（业务模块通常有专属码） |
| 429001 | 429 | ErrTooManyReq | `too many requests` | **后端业务频控 OR nginx per-IP 限流**。指数退避，最多重试 3 次 |
| 500001 | 500 | ErrServer | `internal server error` | 服务器内部错误，附带 `request_id` 给后端排障 |
| 500002 | 500 | ErrDatabase | `database error` | 不同于 500001 的细分。客户端处理一致 |
| 500003 | 500 | ErrCacheError | `cache error` | 同上 |
| 503001 | 503 | ErrServiceUnavail | `service unavailable` | 健康检查不通过 / 主从故障 / 维护中。**1 分钟后重试**。`data` 含子项状态 |

> **客户端 token 刷新流程关键**：只有 401002 才触发 refresh；401003 (`token invalid`) 必须**清掉本地 token 并跳登录**，不要尝试 refresh（refresh 同样会拒）。

---

## 3. 用户 / 鉴权模块（410xxx）

| code | HTTP | 名称 | msg | 触发 |
| --- | --- | --- | --- | --- |
| 410101 | 400 | ErrPhoneFormat | `invalid phone number format` | phone 不符合中国大陆 11 位 |
| 410102 | 400 | ErrCodeFormat | `invalid verification code format` | code 不是 6 位纯数字（**目前 service 层逻辑已用 410104 覆盖大部分场景，410102 较少触发**） |
| 410103 | 400 | ErrCodeNotFound | `verification code not found or expired` | 未发 / 已过期（5 分钟） / 已用过 |
| 410104 | 400 | ErrCodeMismatch | `verification code does not match` | 验证码不匹配（包括错位 + 错值） |
| 410105 | 429 | ErrSMSWindow | `send code too frequently, please wait` | 同 phone 60s 内再次发码 |
| 410106 | 429 | ErrSMSDailyPhone | `daily send limit reached for this phone` | 同 phone 当天 ≥ 5 次 |
| 410107 | 429 | ErrSMSDailyIP | `daily send limit reached for this IP` | 同 IP 当天 ≥ 10 次 |
| 410108 | 404 | ErrUserNotFound | `user not found` | user_id 对应用户不存在 / 已注销 |
| 410109 | 403 | ErrUserBanned | `user is banned` | 账户被封 |
| 410110 | 400 | ErrNicknameInvalid | `invalid nickname` | nickname trim 后为空 |
| 410111 | 400 | ErrBioTooLong | `bio is too long` | bio > 200 字 |
| 410112 | 400 | ErrAvatarURLInvalid | `invalid avatar URL` | avatar_url 格式问题（**主要由 460105 处理白名单不通过的情况**） |

---

## 4. 内容（帖子）模块（412xxx）

> 411xxx 段位预留给用户扩展（账号找回 / 设备管理），目前空闲。

| code | HTTP | 名称 | msg | 触发 |
| --- | --- | --- | --- | --- |
| 412101 | 400 | ErrTitleEmpty | `title cannot be empty` | title trim 后为空 |
| 412102 | 400 | ErrTitleTooLong | `title exceeds 100 characters` | title > 100 字符 |
| 412103 | 400 | ErrSceneTagInvalid | `scene_tag must be between 1 and 8` | scene_tag ∉ [1, 8] |
| 412104 | 404 | ErrPostNotFound | `post not found` | 帖子不存在 / 已被审拒 / is_visible=0（不区分） |
| 412105 | 403 | ErrPostForbidden | `no permission for this post` | （预留，目前 MVP 无编辑/删除接口） |
| 412106 | 400 | ErrCursorInvalid | `invalid cursor` | feed/post-list 的 cursor 非纯数字 |
| 412107 | 400 | ErrPageSizeInvalid | `page size must be between 1 and 50` | size ∉ [1, 50] |
| 412108 | 400 | ErrContentTooLong | `content exceeds 5000 characters` | content > 5000 字符 |

---

## 5. 关注模块（440xxx）

| code | HTTP | 名称 | msg | 触发 |
| --- | --- | --- | --- | --- |
| 440101 | 400 | ErrCannotFollowSelf | `cannot follow yourself` | followerID == targetID |
| 440102 | 400 | ErrFollowLimitExceeded | `following limit reached (max 3000)` | 自己关注数已达 3000 |
| 440103 | 404 | ErrFollowNotFound | `follow relationship not found` | 取关时当前未关注此人（**取关不幂等**） |
| 440104 | 400 | ErrFollowCursorInvalid | `invalid follow list cursor` | followers/following 列表 cursor 非纯数字 |

---

## 6. 搜索模块（450xxx）

| code | HTTP | 名称 | msg | 触发 |
| --- | --- | --- | --- | --- |
| 450101 | 400 | ErrSearchKeywordEmpty | `search keyword cannot be empty` | `q` 为空 / 仅空白 / 仅 MySQL 保留符 |
| 450102 | 400 | ErrSearchCursorInvalid | `invalid search cursor` | search cursor 非纯数字 |

---

## 7. 上传模块（460xxx）

| code | HTTP | 名称 | msg | 触发 |
| --- | --- | --- | --- | --- |
| 460101 | 400 | ErrUploadFileType | `unsupported file type` | content_type 不在 `image/jpeg|png|webp` 白名单 |
| 460102 | 400 | ErrUploadFileTooLarge | `file exceeds size limit` | size > 5 MiB |
| 460103 | 500 | ErrUploadPresignFailed | `failed to generate presigned url` | OSS 签名失败（密钥配错 / 网络） |
| 460104 | 400 | ErrUploadCallbackInvalid | `invalid callback nonce` | nonce 不存在 / 已被消费 / 已过期 |
| 460105 | 400 | ErrUploadURLNotAllowed | `url not in oss whitelist` | cover_url / avatar_url / step.image_url 前缀不等于 `oss.url_prefix` |
| 460106 | 400 | ErrPostStepsInvalid | `post steps invalid` | steps 数组结构非法（数量超 30 / step.text 为空 / image_urls 超 3） |

---

## 8. 审核模块（470xxx，**仅内部日志**）

| code | HTTP | 名称 | msg |
| --- | --- | --- | --- |
| 470101 | 500 | ErrAuditAPIFailed | `content safety API call failed` |
| 470102 | 500 | ErrAuditWriteFailed | `audit result write failed` |

**永远不会**出现在 HTTP 响应里——审核全异步，错误用于日志标签 + Prometheus 告警。

---

## 9. 加密模块（480xxx，**仅内部日志**）

| code | HTTP | 名称 | msg |
| --- | --- | --- | --- |
| 480101 | 500 | ErrEncryptPhone | `phone encryption failed` |
| 480102 | 500 | ErrDecryptPhone | `phone decryption failed` |
| 480103 | 500 | ErrPhoneKeyMissing | `phone encryption key not configured` |

**手机号 AES-GCM 字段加密的基础设施错误**。普通业务流程不会触发——一旦触发说明 `encryption.phone_key` 配错或缺失，是部署问题。客户端只会看到 500001 / 500002，加密错误码用于结构化日志报警。

---

## 10. 端到端"客户端拦截清单"

按"前端要不要弹 toast / 是否引导动作"分类，作为客户端通用错误中间层的实现参考：

| 分类 | 错误码 | 客户端行为 |
| --- | --- | --- |
| **重新登录** | 401003 | 清 token + 跳登录页 |
| **静默 refresh** | 401002 | 调 `/auth/refresh`；refresh 也失败再清 token |
| **频控等待** | 429001 / 410105 / 410106 / 410107 | 显示"操作过快，请稍后"；按指数退避重试 |
| **参数错（开发者锅）** | 400001 / 412106 / 412107 / 440104 / 450102 | toast "请求参数错误"；上报 request_id 给开发 |
| **业务文案** | 410101 / 410103 / 410104 / 410110 / 412101 / 412102 / 412103 / 412108 / 440101 / 440102 / 450101 / 460101 / 460102 / 460105 / 460106 | 直接把 `msg` / 自定义中文描述展示给用户 |
| **资源不存在** | 410108 / 412104 / 440103 | "内容不存在或已被删除"；返回上一页 |
| **服务端错误** | 500001 / 500002 / 500003 / 460103 | toast "服务器繁忙"；展示 `request_id`；可重试 |
| **维护中** | 503001 | toast "服务正在维护"；不要立即重试 |
| **账号问题** | 410109 / 403001 | "账户已被限制"；引导联系客服 |

---

## 11. 添加新错误码的纪律

1. **追加到文件末尾**：在 `pkg/errcode/errcode.go` 对应段位 var 块的末尾追加，**不修改任何已有定义**（错误码是 API 契约的一部分，已发布的 ABI 不可破坏，CLAUDE.md §10）。
2. **段位归属**：用户类的进 410xxx，内容类进 412xxx，等等。如果新模块需要全新段位，先在本文档 §1 登记。
3. **HTTPStatus 显式指定**：永远调 `errcode.New(httpStatus, code, msg)`，**不要**靠 X 段位推。
4. **本表 + 模块文档同步更新**：新增的错误码先写到 `errcode.go`，然后追加到本表对应模块章节 + 各 `0X-*.md` 接口的"Response 失败"列表。
