# 91 · OSS 直传完整流程

> 配套接口文档：[06-upload.md](./06-upload.md)。
>
> 本文档面向前端 / 客户端，给出一个**可复制粘贴**的端到端示例。

---

## 0. 为什么走直传

| 方案 | 流量经过后端？ | 后端 CPU? | 速度 |
| --- | --- | --- | --- |
| 后端转发上传 | 是 | 高（IO + 内存） | 慢（多一跳） |
| **OSS 直传（我们采用）** | **否** | **零** | **快（客户端 ↔ OSS 直连）** |

后端只签 URL + 记账。图片字节客户端**直接** PUT 到 OSS，省一份带宽 + 后端 IO。

---

## 1. 三步流程

```
┌──────────┐                ┌─────────┐                 ┌──────────┐
│ Client   │                │ Backend │                 │ Aliyun   │
│ (Browser │                │ (Go)    │                 │ OSS      │
│  / RN)   │                │         │                 │ (HK)     │
└────┬─────┘                └────┬────┘                 └────┬─────┘
     │                           │                           │
     │ 1. POST /upload/presign   │                           │
     │ {filename,content_type,   │                           │
     │  size,purpose}            │                           │
     ├──────────────────────────▶│                           │
     │                           │                           │
     │                           │ • 生成 object_key         │
     │                           │ • 生成 nonce (16B hex)    │
     │                           │ • Bucket.SignURL()        │
     │                           │ • Redis SET nonce →       │
     │                           │   (uid, public_url, ...)  │
     │                           │   EX 900s                 │
     │                           │                           │
     │ 200 {upload_url, public_  │                           │
     │  url, headers, nonce,     │                           │
     │  expires_at}              │                           │
     │◀──────────────────────────┤                           │
     │                           │                           │
     │ 2. PUT <upload_url>       │                           │
     │    Headers: 原样           │                           │
     │    Body: 图片字节          │                           │
     ├───────────────────────────────────────────────────────▶│
     │                           │                           │
     │                           │                           │ • 校签名
     │                           │                           │ • 校 Content-Type
     │                           │                           │ • 写对象
     │                           │                           │
     │ 200 OK                    │                           │
     │◀───────────────────────────────────────────────────────┤
     │                           │                           │
     │ 3. POST /upload/callback  │                           │
     │ {nonce}                   │                           │
     ├──────────────────────────▶│                           │
     │                           │                           │
     │                           │ • Redis GETDEL nonce      │
     │                           │   → public_url, key       │
     │                           │ • （MVP 不回头验对象）     │
     │                           │                           │
     │ 200 {url, object_key}     │                           │
     │◀──────────────────────────┤                           │
     │                           │                           │
     │ 4. POST /posts            │                           │
     │ {cover_url: <url>, ...}   │                           │
     ├──────────────────────────▶│                           │
     │                           │ • IsAllowedURL 校验前缀    │
     │                           │                           │
     │ 200                       │                           │
     │◀──────────────────────────┤                           │
```

---

## 2. 浏览器（fetch）实现示例

```javascript
// 入参：File 对象 + purpose（avatar / cover / step）
async function uploadImage(file, purpose, accessToken) {
  // ── Step 1: presign ─────────────────────────────────────
  const presignResp = await fetch('https://mellowck.com/api/v1/upload/presign', {
    method: 'POST',
    headers: {
      'Authorization': `Bearer ${accessToken}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      filename: file.name,
      content_type: file.type,        // 必须是 image/jpeg|png|webp
      size: file.size,                // 必须 ≤ 5_242_880
      purpose: purpose,
    }),
  });
  const { data: presign } = await presignResp.json();
  if (presignResp.status !== 200) throw new Error('presign failed');

  // ── Step 2: PUT 直传 OSS ─────────────────────────────────
  const ossResp = await fetch(presign.upload_url, {
    method: 'PUT',
    headers: presign.headers,         // 原样，必带 Content-Type
    body: file,                       // 原始字节，不要 FormData！
    // 不要带 Authorization、不要带 X-Request-ID
  });
  if (ossResp.status !== 200) throw new Error('oss upload failed');

  // ── Step 3: callback 确认 ───────────────────────────────
  const cbResp = await fetch('https://mellowck.com/api/v1/upload/callback', {
    method: 'POST',
    headers: {
      'Authorization': `Bearer ${accessToken}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ nonce: presign.nonce }),
  });
  const { data: cb } = await cbResp.json();
  if (cbResp.status !== 200) throw new Error('callback failed');

  return cb.url;   // 拿去填到帖子 / 头像
}
```

---

## 3. React Native 实现差异点

- File API 不可用。用 `react-native-fs` / `expo-file-system` 读字节。
- `fetch` 的 body 要直接是 Blob / base64 解码后的 Uint8Array；**不要**用 `FormData`。
- Content-Type 需要应用层判断（看后缀）：

```typescript
function inferContentType(localPath: string): string {
  const ext = localPath.split('.').pop()!.toLowerCase();
  switch (ext) {
    case 'jpg': case 'jpeg': return 'image/jpeg';
    case 'png':              return 'image/png';
    case 'webp':             return 'image/webp';
    default: throw new Error('unsupported image type');
  }
}
```

---

## 4. iOS / Android 原生平台要点

- iOS 原图大概率是 HEIC，**必须**用系统 API 转 JPEG 再传（`UIImage.jpegData(compressionQuality:)`）。
- Android 拍照可能拿到不规范方向的 EXIF，传到 OSS 后旋转角会被丢——前端展示时需要兜底处理。
- Content-Length 由 HTTP 客户端自动算。**不要**手填，否则 OSS 偶发 411。

---

## 5. 常见错误与排查

| 现象 | 可能原因 | 排查 |
| --- | --- | --- |
| presign 报 460101 | content_type 不在 jpeg/png/webp | 客户端转格式 |
| presign 报 460102 | size > 5 MiB | 压缩后再传 |
| presign 报 460103 | OSS 密钥配错 / 网络断 | 后端日志看 OSS SDK 错误 |
| presign 报 429001 | 1h 内已 30 次 | 等几分钟或减少试错 |
| PUT 报 OSS 403 SignatureDoesNotMatch | Content-Type 不一致 / 没把 headers 原样带上 / URL 中的 `Expires` 已过 | 重新 presign，**不要复用 URL** |
| PUT 报 OSS 403 RequestTimeTooSkewed | 客户端时钟与服务器差 > 15 分钟 | 同步系统时间 |
| PUT 报 OSS 411 LengthRequired | RN/原生客户端没自动 Content-Length | 让 HTTP 库自己算，不要手设 |
| PUT 报 OSS 404 NoSuchBucket | 配错 endpoint（试图用国内访问香港 bucket） | 确认 `endpoint=oss-cn-hongkong.aliyuncs.com` |
| callback 报 460104 | nonce 不存在 / 已用 / 已过期 | 重新 presign + PUT + callback |
| callback 成功后图片 404 | PUT 实际失败但客户端误判成功 | 前端必须严格 `ossResp.status === 200` 才往下走 |
| 发帖报 460105 | cover_url / image_url 前缀不在白名单 | 必须是 `https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/...`（生产）/`http://127.0.0.1:18080/...`（dev mock） |

---

## 6. URL 前缀（白名单）

接口写入侧（avatar_url、cover_url、step.image_urls）的 URL 前缀必须命中 `oss.url_prefix`：

| 环境 | url_prefix |
| --- | --- |
| dev (`provider=mock`) | `http://127.0.0.1:18080/` |
| prod (`provider=aliyun`) | `https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/` |

检查实现：`pkg/oss/whitelist.go::IsAllowedURL`（**大小写不敏感**前缀比对；空 URL 永远允许通过——用于"清空头像"场景）。

---

## 7. 安全边界

| 风险 | 现状 |
| --- | --- |
| 客户端伪造其他人的 public_url | 不可能——presign 时 nonce 与 user_id+public_url 绑定写 Redis，callback 时由服务端从 Redis 拿回 |
| 客户端 PUT 一张色情图后调 callback 拿到 URL | **会成功**——OSS 不审图。但后续帖子审核（07-audit.md）会拦下含色情封面图描述的帖子。**图片本身今天不送审** |
| 上传 5 MiB 以上的图绕过 presign size 校验 | 不可能——`Bucket.SignURL` 不绑 size，但 OSS 服务端可设 `Content-Length` 上限。**目前 OSS 桶级配置未限大小**，理论上恶意客户端可以传 100 MiB；后续遗留债务 |
| 直传成功但帖子没发出去 = 孤儿对象 | 是。MVP 不清理，后续运维脚本处理 |
| 拿到 upload_url 后泄漏出去 | 影响有限——签名 URL 15 分钟过期 + 只允许写一次 + 一个 object_key |
