# 06 · 上传模块（OSS 直传）

> 代码来源：`internal/handler/upload.go`、`internal/service/upload_service.go`、`internal/model/dto/upload.go`、`pkg/oss/{client,aliyun,whitelist}.go`。
>
> **整体流程图与示例代码**：见 [91-oss-upload.md](./91-oss-upload.md)。

---

## 设计要点

图片**不经过后端**，客户端直传 OSS：

```
1. 客户端 → 后端：POST /upload/presign   申请一次性 PUT URL
2. 客户端 → OSS ：PUT <upload_url>       直接传字节
3. 客户端 → 后端：POST /upload/callback  确认 nonce 拿到 public_url
4. 客户端 → 后端：把 public_url 填进帖子 / 头像字段
```

后端**永远不接收图片字节**。`cover_url` / `avatar_url` / `step.image_urls` 都必须是从 step 3 拿到的 `public_url`，否则 OSS 白名单（`oss.url_prefix`）会拒掉。

---

## 1. POST `/api/v1/upload/presign` —— 申请直传 URL

**鉴权**：**必须**（+ 每用户 30 次 / 1h 频控，命中报 `429001`）

**Request Body**（`dto.PresignReq` @ `dto/upload.go:32`）

| 字段 | 类型 | 必填 | 校验 |
| --- | --- | --- | --- |
| `filename` | string | 是 | `required,max=200`。仅用于扩展名兜底，OSS object_key 不会原样保留它 |
| `content_type` | string | 是 | **白名单三选一**：`image/jpeg` / `image/png` / `image/webp`。**不接受 HEIC / GIF / SVG** |
| `size` | int64 | 是 | `required,min=1,max=5242880`（5 MiB）。**前端必须自己测字节数**再请求 |
| `purpose` | string | 是 | **白名单三选一**：`avatar` / `cover` / `step`。决定 object_key 前缀 |

**Response 成功**（`dto.PresignResp` @ `dto/upload.go:45`）

```json
{
  "code": 0,
  "data": {
    "upload_url": "https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/cover/1001/202605/abc-uuid.jpg?Expires=...&OSSAccessKeyId=...&Signature=...",
    "public_url": "https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/cover/1001/202605/abc-uuid.jpg",
    "method": "PUT",
    "headers": { "Content-Type": "image/jpeg" },
    "nonce": "a1b2c3d4e5f6...（32 字符 hex）",
    "expires_at": 1714201800000
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `upload_url` | **15 分钟内**（`oss.presign_ttl`）有效的预签名 PUT URL |
| `public_url` | 直传成功后帖子里要填的最终 URL。**`presign` 阶段已经知道**——上传成功后无需 callback 也能拿到展示用 URL |
| `method` | 始终是 `"PUT"`，前端按这个值发请求 |
| `headers` | **必须原样**带到 PUT 请求里，否则 OSS 返回 403 SignatureDoesNotMatch（特别是 `Content-Type` 必须与签名时绑定的一致） |
| `nonce` | 16 字节随机数的 hex（32 字符），后续 callback 用。**禁止前端自行解析或拼装** |
| `expires_at` | UnixMilli 直传 URL 过期时间。**`upload_url` 与 `nonce` 都失效**，要重新调 presign |

**object_key 形状**（仅供调试，前端无需关心）：

```
{purpose}/{user_id}/{yyyymm}/{uuid}.{ext}
例：cover/1001/202605/abc-uuid-here.jpg
```

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | JSON 体格式错 / 基础校验失败 |
| 460101 | 400 | `content_type` 不在白名单 |
| 460102 | 400 | `size` 超 5 MiB |
| 460103 | 500 | OSS 签名失败（密钥配错 / 网络问题） |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |
| 429001 | 429 | 1h 内已请求 30 次 |

**cURL**

```bash
curl -i -X POST https://mellowck.com/api/v1/upload/presign \
  -H 'Authorization: Bearer <ACCESS_TOKEN>' \
  -H 'Content-Type: application/json' \
  -d '{
    "filename": "lunch.jpg",
    "content_type": "image/jpeg",
    "size": 524288,
    "purpose": "cover"
  }'
```

---

## 2. PUT `<upload_url>` —— 客户端直传 OSS（**不经后端**）

**目标**：把图片字节传到 OSS。

```javascript
// 浏览器示例
await fetch(presignResp.data.upload_url, {
  method: 'PUT',
  headers: presignResp.data.headers,   // 原样
  body: fileBlob                        // 原始字节，不要 FormData 包一层
});
```

**关键约束**：

- **必须用 PUT，不是 POST**。
- **不要包 multipart/form-data**，原始字节 body。
- **`Content-Type` 必须与 `headers.Content-Type` 完全一致**，否则 403。
- **不要带 `Authorization` 头**（OSS 不需要，且签名已经把权限带进 URL 了）。
- 这一步成功（HTTP 200）后**可以**直接渲染 `public_url`，但服务器还没记账，必须继续 step 3。

---

## 3. POST `/api/v1/upload/callback` —— 上传确认

**鉴权**：**必须**

**Request Body**（`dto.CallbackReq` @ `dto/upload.go:64`）

| 字段 | 类型 | 必填 | 校验 |
| --- | --- | --- | --- |
| `nonce` | string | 是 | `required,min=16,max=128`（实际取 step 1 拿到的 32 字符 hex） |

**注意**：不接受客户端传 `url`——服务端在 presign 时已经把 `public_url` 跟 nonce 绑定存到 Redis，避免客户端伪造第三方 URL 绕过白名单。

**Response 成功**（`dto.CallbackResp` @ `dto/upload.go:74`）

```json
{
  "code": 0,
  "data": {
    "url": "https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/cover/1001/202605/abc-uuid.jpg",
    "object_key": "cover/1001/202605/abc-uuid.jpg"
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `url` | 与 presign 阶段的 `public_url` 完全一致，用于填到帖子 / 用户资料 |
| `object_key` | OSS 对象键。客户端排障时用 |

**实现细节**：

- nonce 走 Redis **GETDEL 原子消费**——一旦 callback 成功，同一个 nonce 不能再用。
- 服务端**不**回头去 OSS HEAD 验证图片真的传上来了（信任客户端在 PUT 200 后才会调 callback）。如果 PUT 失败但客户端误调了 callback，public_url 指向的对象不存在——后续 GET 时 OSS 返回 404，前端要兜底显示占位图。
- 服务端**不**校验 nonce 是当前用户在 presign 阶段申请的——理论上 user_A 可以拿 user_B 的 nonce 调 callback，但 nonce 是 128 位随机熵，**实际不可猜**。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | JSON 体格式错 |
| 460104 | 400 | nonce 不存在 / 已被消费 / 已过期 |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |

---

## 4. 把 URL 填进帖子 / 头像 / 步骤图

调完 callback 后，把 `url` 字段填到对应业务接口：

| 用途 | 接口 / 字段 |
| --- | --- |
| 头像 | `PATCH /users/me` 的 `avatar_url` |
| 帖子封面 | `POST /posts` 的 `cover_url` |
| 步骤图 | `POST /posts` 的 `steps[].image_urls[]` |

后端会用 `oss.IsAllowedURL` 校验前缀必须等于 `oss.url_prefix`（prod 是 `https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/`）。**写入第三方 URL 会被 `460105 ErrUploadURLNotAllowed` 拒掉**。

清空头像：`PATCH /users/me` 的 `avatar_url: ""`（空字符串显式允许）。

---

## 5. 容量上限与错误恢复

| 场景 | 客户端应对 |
| --- | --- |
| 图片 > 5 MiB | 调 presign 前**压缩到 5 MiB 以下**，不要靠服务器报 460102 才发现 |
| PUT 阶段网络断 | 重新调 presign 拿新 nonce + 新 upload_url。**不要复用旧 nonce**——它已经绑定了过期的 URL |
| callback 阶段网络断 | 在 15 分钟内（presign TTL）可以重试 callback。超时则全部从 presign 重做 |
| 用户改头像但不需要传新图 | **不要**调 upload。`PATCH /users/me` 直接发 `{"avatar_url":"<旧 URL>"}` 即可（旧 URL 也在白名单内） |

---

## 6. 不存在的接口

| 路径 | 状态 | 说明 |
| --- | --- | --- |
| `POST /api/v1/upload/image`（直接上传字节） | **不存在** | 走 presign + 直传 |
| `DELETE /api/v1/upload/:object_key` | **不存在** | MVP 不支持删除已传图片；后续清理由人工通过 OSS 控制台或运维脚本处理 |
| `GET /api/v1/upload/list` | **不存在** | 客户端自行记录 |
