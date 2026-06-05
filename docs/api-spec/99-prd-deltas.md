# 99 · 代码 vs PRD 偏离点登记

> 本系列文档以**真实代码**为准（截止 2026-05-23，main 分支 commit `233024e`）。
>
> 凡是与 `docs/prd/PRD-Final-v3.0.md` 不一致的地方，逐条记到本文件，作为下一版 PRD 修订的唯一权威输入。

---

## A. 鉴权 / 用户

### A1. 自动注册取代独立注册接口

| 维度 | 内容 |
| --- | --- |
| PRD 主张 | 注册与登录为独立两步流程 |
| 实际实现 | `POST /api/v1/auth/login` 收到不存在的 phone 时直接创建账号（`user_service.go:171`），昵称默认 `"厨友" + phone 末 4 位` |
| 影响 | 没有独立 `POST /auth/register` 接口；前端只需对接 send-code + login 两个接口 |
| 选择此方案的原因 | 短信码已经验证了手机号所有权，无需再多一次"注册确认"动作。减少前端状态机分支 |

### A2. 短信发送接口路径

| 维度 | 内容 |
| --- | --- |
| PRD 早期草稿 | `/auth/send-sms` 或 `/sms/send` |
| 实际实现 | `POST /api/v1/auth/send-code` |
| 影响 | 前端必须以 `send-code` 为准 |

### A3. 无明文手机号读取接口

| 维度 | 内容 |
| --- | --- |
| PRD | 提到"我的资料"展示手机号 |
| 实际实现 | `GET /users/me` 只返回 `phone_masked`（`138****8000`），无任何接口能拿到明文 |
| 选择此方案的原因 | 手机号 AES-GCM 字段加密（Step 11），解密 API 是内部基础设施，不暴露 HTTP 端点 |

### A4. 无注销账户接口

| 维度 | 内容 |
| --- | --- |
| PRD | 用户可注销账户 |
| 实际实现 | 没有 `DELETE /users/me`。被封禁通过管理员改库（`is_banned=1`）实现 |
| 选择此方案的原因 | MVP 范围 |

### A5. 鉴权 / 上传频控的具体值

| PRD | 实际（`cmd/server/main.go:436-507`） |
| --- | --- |
| SMS 每 phone 每分钟 1 次 | 一致 |
| SMS 每 phone 每天 5 次 | 一致 |
| SMS 每 IP 每天 10 次 | 一致 |
| 发帖每用户每天 N 次（未定） | **20 次 / 24h** |
| 点赞每用户每天 N 次（未定） | **200 次 / 24h** |
| 搜索每 IP 限流（未定） | **30 次 / 60s** |
| 上传 presign 限流（未定） | **30 次 / 1h** |

---

## B. 内容 / 帖子

### B1. 新发帖默认不可见，等审核回写

| 维度 | 内容 |
| --- | --- |
| PRD 主张 | 发帖立即可见（Step 4 MVP），后置审核异步 |
| 实际实现 | **Step 10 起改为 fail-closed**——`POST /posts` 立即返回 `audit_status=0, is_visible=0`，等 AuditConsumer 改回 `audit_status=1, is_visible=1` 后才在 Feed 可见 |
| 影响 | 前端发帖后**必须轮询详情**才知道是否已可见。详见 [07-audit.md §5.2](./07-audit.md) |
| 选择此方案的原因 | 内容安全合规优先；mock 环境 0-300ms 通过，prod 5-10s 内通过，体感影响可接受 |

### B2. 帖子作者本人也看不到自己未通过的帖

| 维度 | 内容 |
| --- | --- |
| PRD | 作者本人可看到自己 pending / rejected 的帖 |
| 实际实现 | `GET /posts/:id` 对 `is_visible=0` 一律返回 412104，**不区分作者本人**。`post.go` 注释承诺"Step 10 task"补全，实际未补 |
| 影响 | 前端"我的发布"列表无法展示审核中的帖 |
| 修复路线 | 需要新增 optional auth 中间件 + service 层加 viewerID 判断，遗留债务 |

### B3. 无编辑 / 删除帖子接口

| 维度 | 内容 |
| --- | --- |
| PRD | 列出帖子编辑 / 删除 |
| 实际实现 | 既无 `PATCH /posts/:id` 也无 `DELETE /posts/:id` |
| 选择此方案的原因 | MVP 简化决策（评论 / 私信 / 直播也都不做，见 CLAUDE.md §4.5） |

### B4. PRD 未明确 content 字段长度，实际 5000

实际限制 `binding:"max=5000"`（`dto/post.go:63`），migration 用 `TEXT` 列。

---

## C. 关注

### C1. 取关不幂等（核心偏离）

| 维度 | 内容 |
| --- | --- |
| PRD 主张 | "重复取关应幂等" |
| 实际实现 | 取关未关注的人返回 `440103 ErrFollowNotFound (HTTP 404)` |
| 影响 | 前端 DELETE 调用必须确认当前确实关注着 |
| 选择此方案的原因 | 客户端 UI 只在"已关注"态显示取消按钮，幂等无必要；明确报错有助于发现客户端状态不一致 bug |

### C2. 列表不返回 followed_at

| 维度 | 内容 |
| --- | --- |
| PRD | 列表可能展示关注时间 |
| 实际实现 | `UserBrief` 只有 id / nickname / avatar_url，**无时间字段** |
| 选择此方案的原因 | 产品最终决定列表只渲染头像+昵称，避免"互关时间"暗示社交压力 |

### C3. 列表分页用 follows.id 而非 followed_at

cursor 内部编码是 `follows.id` 十进制字符串（`follow_service.go:374`），**前端禁止解析**。

### C4. 关注数 3000 上限

`follow.max_following = 3000`（`config.prod.yaml:151`）。PRD 早期数字 1000，实际放宽到 3000。

---

## D. 点赞

### D1. 允许自赞

PRD 提议禁自赞，实际 service 层**未做检查**。产品定稿是"不禁，让用户自己定义"。

### D2. 重复操作不发 MQ

第二次 like 同一帖 / 第二次 unlike 已未点过的帖**不发布事件**（`like_service.go` SISMEMBER 短路）。这是 PRD 未明确的实现细节，对前端透明（响应仍正确）。

### D3. 返回值 count 取 Redis 实时值，不取 MySQL

`like_count` 字段在 `LikeResp.count` 里来自 Redis `like:cnt:{post_id}`（INCR 后立即返回），与 `posts.like_count` 列脱钩（后者最多滞后 3s）。前端用 `LikeResp.count` 立即更新 UI 即可。

---

## E. 搜索

### E1. Query 名是 `q` 不是 `keyword`

PRD 用 `keyword`，代码 `form:"q"`（`dto/search.go:47`）。前端必须用 `q`。

### E2. 关键词超长**截断**不**报错**

PRD AC-7 要求"超长截断"，实际实现：

- 超过 `search.max_keyword_len`（prod 50）按 **rune**（非 byte）截断到 50 字
- 不报错，按截断后的关键词搜

### E3. 分页用 OFFSET，不用游标

`search_service.go:209` cursor 实际是十进制 OFFSET。深翻页性能会逐渐变差。前端**禁止做"跳页"UI**。

---

## F. 上传

### F1. 三种文件类型白名单

只接受 `image/jpeg`、`image/png`、`image/webp`。**HEIC / GIF / SVG 全部不接受**：

- HEIC：iOS 默认格式，需服务端转换，MVP 不做
- GIF / SVG：安全攻击面（动图大小、内嵌脚本）

PRD 没有明确，实际固定到这三种。

### F2. 图片不送审

PRD 提到图片审核，实际 Step 10 只送文本（title + content + steps.text）给 Aliyun Green。**图片本身不做内容安全校验**——遗留债务。

### F3. 客户端补 url 字段被拒绝

`CallbackReq` 只接受 `nonce`，**不接受** `url`——服务端从 Redis 拿绑定的 public_url。安全考虑。

### F4. 5 MiB 上限

`max_image_size: 5242880`（`config.prod.yaml:95`）。PRD 未明确。

---

## G. 响应 / 协议

### G1. 时间字段全部用 UnixMilli int64

PRD 早期讨论过 ISO 8601 字符串，实际全部采用毫秒整数（`dto/post.go:14-17` 显式注释决策）。

### G2. `next_cursor` + `has_more` 双重信号

两个字段都标记"是否有下一页"，**冗余设计**（客户端检查 `next_cursor != ""` 或 `has_more` 都可以）。是显式约定。

### G3. Cursor 不透明

所有列表的 `next_cursor` 都是不透明字符串——feed 用毫秒时间戳、search 用 OFFSET、follow 用 follows.id。**客户端禁止解析任何 cursor**，原样回传。

### G4. `request_id` 透传

请求头 `X-Request-ID` 没传时后端生成 UUIDv4；响应体的 `request_id` 字段 + 响应头 `X-Request-ID` 都返回。**排障时把这个值给后端，能直达日志**。

### G5. 业务 code 与 HTTP status 双轨

成功永远 HTTP 200 + `code: 0`；失败 HTTP 4xx/5xx + 业务 code（如 410105）。**前端不要靠 HTTP code 推业务含义**，永远看响应体的 `code`。

---

## H. 部署 / 配置层面

### H1. prod 的 `audit.provider` 实际仍是 `mock`

`configs/config.prod.yaml:60` 的字段值是 `provider: mock`，注释写"Production uses real Aliyun SMS"——这是 Step 10 完成时留的占位，go-live 前必须切换到 `aliyun`。截至 2026-05-23 仍未切（生产环境的帖子目前实际上无内容审核）。

### H2. CORS allowed_origins 占位待替换

`config.prod.yaml:159` 是 `https://cooking.example.com`——上线时改 `https://mellowck.com`。**`*` 通配在 release 模式会被 `config.Validate` 拒启动**（CLAUDE.md §4 红线）。

### H3. `/metrics` 路由公开但 nginx 不外暴露

代码层 `/metrics` 不做鉴权（`cmd/server/main.go`），生产由 nginx 不暴露此 location 来兜底。

---

## I. 总结：客户端需调整的核心点

| 点 | 调整方向 |
| --- | --- |
| 发帖即可见 | 改成"发帖后轮询详情拿到 audit_status ∈ {1,3} 才视为发布成功" |
| 注册 | 移除独立注册流程，统一走 send-code → login |
| SMS 接口名 | `send-code`（不是 `send-sms`） |
| 取消关注 | 仅在"已关注"态发起 DELETE，否则前端拦截 |
| 搜索 query | 名为 `q` 不是 `keyword` |
| 图片格式 | iOS 端必须本地转 HEIC → JPEG |
| 401 处理 | 严格区分 401002 (refresh) vs 401003 (重新登录) |
| 列表 cursor | 不要解析，原样回传 |
| 时间格式 | 全部按 UnixMilli 处理 |
