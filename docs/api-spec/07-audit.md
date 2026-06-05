# 07 · 内容审核状态机（无独立接口）

> 代码来源：`internal/consumer/audit_consumer.go`、`internal/model/post.go:71-76`、`pkg/audit/{auditor,aliyun,mock}.go`、`internal/service/post_service.go`。

---

## 0. 重要提示

**审核没有任何对外 HTTP 接口**。`audit_status` 是 `dto.CreatePostResp` / `dto.PostDetailResp` 里的一个字段，由后端异步维护，前端**只读**。本文档解释这个字段每个值的含义、状态机的迁移路径，以及前端 / 客户端 UI 该如何对应。

---

## 1. 字段定义

`audit_status` (uint8) — 来自 `internal/model/post.go:71-76`：

| 值 | 常量名 | 含义 | 配套 `is_visible` |
| --- | --- | --- | --- |
| 0 | `AuditStatusPending` | 待审（机审还没回写） | 0 |
| 1 | `AuditStatusMachinePass` | 机审通过 | **1** |
| 2 | `AuditStatusSuspect` | 机审疑似——进入人工队列 | 0 |
| 3 | `AuditStatusManualPass` | 人工通过 | **1** |
| 4 | `AuditStatusMachineDeny` | 机审拒绝 | 0 |
| 5 | `AuditStatusManualDeny` | 人工拒绝 | 0 |

**规则**：`is_visible == 1` ⇔ `audit_status ∈ {1, 3}`。

---

## 2. 状态机

```
                    POST /api/v1/posts
                          ↓
                  audit_status = 0
                  is_visible   = 0
                          │
                          │ (异步：AuditConsumer 调 Aliyun Green 或 mock)
                          ↓
        ┌─────────────────┼─────────────────┐
        ↓                 ↓                 ↓
   status=1          status=2          status=4
   (机审通过)        (机审疑似)        (机审拒绝)
   visible=1         visible=0         visible=0
   【终态A】         │                 【终态B】
                    │
                    │ (人工复审)
                    ↓
              ┌─────┴─────┐
              ↓           ↓
         status=3      status=5
         (人工通过)    (人工拒绝)
         visible=1     visible=0
         【终态C】     【终态D】
```

**当前 MVP 没有人工复审入口**——状态 2 实际上是个停滞终态，需要管理员手动改库。状态 3 / 5 是预留位。

---

## 3. dev 环境的快速通过

`audit.provider=mock`（dev / 默认 prod 占位）：

- mock 实现位于 `pkg/audit/mock.go`。
- 默认 `audit.mock_result=""`：**所有帖子直接 pass**（`audit_status=1`），延迟约 0-300ms（模拟 API RTT）。
- 把 `audit.mock_result` 设成 `"suspect"` / `"reject"` 可强制走 2 / 4 路径（仅本地联调用）。

---

## 4. prod 环境的真审核

`audit.provider=aliyun`（**截至 2026-05-23 prod 配置实际仍是 mock**，见 `configs/config.prod.yaml:60` 注释 "Production uses real Aliyun SMS" 但实际 provider 字段值是 `mock`——这是 Step 10 完成时留的临时占位，go-live 前需要切换）：

- 调用 Aliyun 内容安全 Green API（`pkg/audit/aliyun.go`）。
- 单次 RTT ≈ 200-800ms，per-call timeout 3s（`audit.timeout`），失败重试 2 次（`audit.max_retries`）。
- Aliyun 三档输出 → 我们的四个终态映射：
  | Aliyun 返回 | 我方 audit_status |
  | --- | --- |
  | `pass` | 1（机审通过） |
  | `review` | 2（疑似） |
  | `block` | 4（机审拒绝） |

**审核失败 fail-closed**：API 超时 / 余额不足 / 网络断 → 帖子保持 `audit_status=0`，**永不自动放出**。MVP 没有自动重排队列，需要管理员人工 requeue。

---

## 5. 前端 / 客户端如何使用 audit_status

### 5.1 公开浏览（Feed / 搜索 / 别人主页）

- 永远拿不到 `audit_status ∈ {0, 2, 4, 5}` 的帖子——它们的 `is_visible=0` 被列表过滤掉。
- 拿到的帖子 `audit_status` 必然是 `1` 或 `3`，前端**不需要展示**这个字段。

### 5.2 用户本人查看自己的帖子

**当前 MVP**：本人也只能通过 `GET /posts/:id` 查，而 service 层对 `is_visible=0` 一律返回 404（PRD 偏离点，见 99-prd-deltas.md）。所以**本人也看不到自己被拒 / 待审的帖**。

发完帖之后，前端的合理处理是：

```
1. POST /posts → 拿到 post_id + audit_status=0
2. 前端本地保存"刚发布的草稿"清单
3. 每隔 3s 轮询 GET /posts/<post_id>：
   - 200 → 审核已过（audit_status ∈ {1, 3}）→ 从草稿清单移除，跳转详情
   - 404 → 仍在审核 / 已被拒；继续轮询
4. 总轮询时间上限 30s（prod Aliyun 典型 5-10s 出结果，超过 30s 视为拒/疑似）
5. 30s 后 404 仍在 → 提示用户"内容审核未通过或仍在处理中，请稍后查看"
```

### 5.3 列表里突然出现 / 消失

`audit_status` 翻转后，AuditConsumer 会 `BumpFeedVersion()` 让 Feed 缓存版本失效。所以：

- 帖子审核通过后**最迟 `cache.feed_cache_ttl`（prod 5 分钟）**会出现在公开 Feed 里，实际典型 < 30s。
- 后台人工把已通过的帖改成拒绝（操作 DB），同样最多 5 分钟内列表消失。

---

## 6. 审核内容范围

AuditConsumer 在 `pkg/audit/aliyun.go` 里把以下字段拼成一个文本提交给 Green：

- `title`
- `content`
- 所有 `steps[].text`

**图片不送审**（MVP 简化决策；后续要补图片审核需在 OSS 直传成功后单独发 ImageAuditEvent，目前无此 topic）。

---

## 7. 错误码（仅内部日志 / Prometheus 告警，永不返给客户端）

| code | 含义 |
| --- | --- |
| 470101 | `ErrAuditAPIFailed` — Green API 调用失败（网络 / 余额 / 超时） |
| 470102 | `ErrAuditWriteFailed` — audit_log 表写入失败 |

这两个错误**不会出现在任何 HTTP 响应里**——审核全异步。它们用于日志结构化标签 + Grafana 告警阈值。

---

## 8. 不存在的接口

| 路径 | 状态 | 说明 |
| --- | --- | --- |
| `GET /api/v1/posts/:id/audit-log` | **不存在** | 审核记录不对外暴露 |
| `POST /api/v1/posts/:id/appeal` | **不存在** | MVP 没有申诉渠道 |
| `GET /api/v1/users/me/pending-posts` | **不存在** | MVP 没有"我待审帖子"列表（见 §5.2 轮询方案） |
