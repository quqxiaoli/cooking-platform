# 后端 follow-up · 前端 Phase 1 完工后

> **受众**:后端项目 `/opt/cooking-platform/` 的维护者(包括 Claude Code)。
> **来源**:前端项目 `cooking-frontend` 在 `phase-1-done` 后的衔接文档。
> **目的**:列出前端 Phase 1 上线**后**待后端补的接口。前端代码已按本文档预期格式做了**优雅降级**,后端接口落地前不会炸,落地后**前端零改动**自动用真值。

姊妹文档:`docs/handoff/backend-changes-pre-phase-1.md`(Phase 1 启动前的改造清单,已完工)。

---

## 0. 概览

| 项 | 标签 | 阻塞? | 来源 |
|---|---|---|---|
| 1. `GET /users/:id` 响应加 `is_following: bool` | 🟡 建议 | 不阻塞(前端 graceful) | `DEBT_LEDGER` D-007 |
| 2. `FeedResp.posts[]` 加 `liked_by_me: bool`(影响 `/feed` / `/users/:id/posts` / 搜索) | 🟡 建议 | 不阻塞(前端 graceful) | `DEBT_LEDGER` D-007 |

**共 2 项**,根因同源(列表 / 单点查询 缺"当前用户对此对象的状态"字段)。其它跨仓库联动在 Phase 1 内已全部解决。

---

## 1. `GET /users/:id` 响应加 `is_following: bool`

### 1.1 背景

前端 `FollowButton` 在用户主页 / 详情页作者 / 关注列表 / 粉丝列表展示"关注 / 已关注"切换。当前**无法**取得"当前登录用户是否已关注目标用户 X"的真值——后端 API spec(`docs/api-spec/05-follow.md`)只有:

- `POST /users/:id/follow`(幂等)
- `DELETE /users/:id/follow`(幂等)
- `GET /users/me/following`(我关注的列表,cursor 分页,最多 3000)
- `GET /users/me/followers`(我的粉丝列表)

**没有**"单点查询是否关注"接口。前端在用户主页拉一遍全量 `/users/me/following` 来反查是不划算的(3000 项 60 页,每个详情页都拉一遍 = 60 次请求)。

### 1.2 前端当前兜底

`FollowButton` 初始态固定传 `initialFollowing={false}`,POST / DELETE 走幂等 + 440103 兜底。**结果**:用户进任意非自己的用户主页 / 详情页,即便已关注该用户,按钮仍**显"关注"**——点一次又会触发一次幂等 POST(后端 OK,但 UX 错)。

### 1.3 后端方案 A(推荐)

`GET /api/v1/users/:id` 响应体加 `is_following: bool` 字段。

**契约**:

```jsonc
// 请求(原样,无变化):
GET /api/v1/users/123
Authorization: Bearer <access>

// 响应(diff 在 data 内加一个字段):
{
  "code": 0,
  "msg": "ok",
  "data": {
    "id": 123,
    "nickname": "alice",
    "avatar_url": "https://...",
    "bio": "...",
    "post_count": 42,
    "follower_count": 100,
    "following_count": 50,
    "created_at": 1714200000000,
    // ── 新增 ──────────────────────────────
    "is_following": true   // 当前 access token 标识的用户是否已关注此用户
    // ─────────────────────────────────────
  },
  "request_id": "..."
}
```

**语义**:
- 仅当请求带有效 access token(`Authorization: Bearer ...`)时返回真值
- 匿名请求(无 token / 401003)→ 字段省略(响应不含 `is_following`)**或** 返 `false`,任一均可——前端已写 `.optional()` 兼容
- 请求自己(`:id == me.id`)→ `is_following: false` 即可(前端不会渲染"关注自己"按钮)
- 查询性能:Redis 维护 `follow:<follower_id>:<followee_id>` 单 key,O(1) GET。或 MySQL 主键查 `follows(follower_id, followee_id)` 联合索引一次

### 1.4 后端方案 B(替代,不推荐)

新 `POST /api/v1/users/follow/check` 接受 `{ user_ids: number[] }`,返 `{ [id]: bool }`。

**何时选 B**:Feed / 用户列表批量场景也要展示关注态时。当前 PRD 未要求 Feed 展示作者关注态,**用不上**;方案 A 已足够。

### 1.5 前端落地动作(后端接口上线后)

后端 ship 当天,前端这边的解债极简:

1. `src/features/user/types.ts` 的 `userProfileSchema` 已写 `is_following: z.boolean().optional().default(false)` —— 无需改 schema
2. `src/app/u/[id]/page.tsx` / 详情页 SC 把 `profile.is_following` 透传到 `FollowButton` 的 `initialFollowing` —— **若**当前还在传 `false` 字面量,改为 `profile.is_following`
3. `DEBT_LEDGER` D-007 从 `open` 改 `resolved`,session log 引用本文档 1.3

前端预计改动:≤ 5 行代码 + 1 行 DEBT_LEDGER 状态。

### 1.6 是否必须?

**不必须**。当前 UX 错(已关注用户显"关注")是已知占位,用户行为不会因此被阻断(POST 幂等,真按一下不会重复关注)。本项可放到 Phase 2 第一批解决,亦可单独立项快速 ship。

后端 owner / Claude Code 自行决定优先级。前端这边写完 handoff doc 后即视 D-007 在 `phase-1-done` 闸门处的"已交接"——不算 open(条目会在前端 DEBT_LEDGER 标 `resolved`,引用本文档作"已转后端跟进"的依据)。

---

## 2. `FeedResp.posts[]` 加 `liked_by_me: bool`

### 2.1 背景

前端 `LikeButton` 在帖子卡片(Feed / 用户主页 / 搜索结果)和详情页展示"红心 / 灰心 + 计数"。**详情页**因有专门的 `GET /api/v1/posts/:id/like-status`(`docs/api-spec/03-like.md`)拿真值,已经正确;**列表页**没有对应字段,只能默认 `initialLiked={false}`。

**症状**:用户在详情页点赞后,回到 Feed / 用户主页 / 搜索结果同一帖子卡片,**心仍灰**;再点会触发幂等 POST(后端 OK,count 不变),前端乐观 +1 显示 2,刷新页面又回 1。

### 2.2 前端当前兜底

`PostCard` / `UserPostList` 等卡片组件统一 `initialLiked={false}`(`features/user/components/UserPostList.tsx` 注释引用 D-007)。点赞按钮幂等 + 440101("已点赞")兜底,数据一致性最终态正确——但**UX 显示态在第一次进列表页时一定是错的**(如果用户之前点过赞)。

### 2.3 后端方案 A(推荐)

`FeedResp.posts[]` 每个 PostBrief 加 `liked_by_me: bool` 字段。**一处改 dto,三处接口受益**:

- `GET /api/v1/feed`(`02-post.md §3`)
- `GET /api/v1/users/:id/posts`(`01-user.md §8`)
- `GET /api/v1/search/posts`(`04-search.md`)

**契约**(以 `/feed` 为例):

```jsonc
GET /api/v1/feed?size=20
Authorization: Bearer <access>

// 响应(diff 在 posts[] 每项内加一个字段):
{
  "code": 0,
  "data": {
    "posts": [
      {
        "id": 1001,
        "title": "出租屋一锅炖",
        "scene_tag": 1,
        "scene_name": "出租屋",
        "cover_url": "https://...",
        "like_count": 12,
        "view_count": 200,
        "author": { "id": 1001, "nickname": "厨友8000", "avatar_url": "" },
        "created_at": 1714200000000,
        // ── 新增 ──────────────────────────────
        "liked_by_me": true   // 当前 access token 标识的用户是否已点赞此帖
        // ─────────────────────────────────────
      }
    ],
    "next_cursor": "1714199000000",
    "has_more": true
  }
}
```

**语义**:
- 仅当请求带有效 access token 时返真值;匿名请求字段省略或全为 `false`,任一均可——前端 schema 会写 `.optional()`
- 实现可用 Redis pipeline 批量 GET `post_like:<post_id>:<user_id>` 单 key(每帖 O(1)),N 帖共 N 次 GET 一次 pipeline 即可
- 或 MySQL 单查 `post_likes(user_id, post_id)` 联合索引 IN 一次拉回当前页 N 个 post_id 的命中集合

### 2.4 后端方案 B(替代,不推荐)

新 `POST /api/v1/posts/like-status/batch` 接 `{ post_ids: number[] }`,返 `{ [id]: bool }`。

**何时选 B**:列表页加载后**异步**补点赞态。但这会引入两阶段渲染——前端要先渲灰心 + 收到 batch 响应后再 patch,UX 闪烁,**反而不如**方案 A 一次 RTT 拉齐。

### 2.5 详情页(`GET /api/v1/posts/:id`)

详情页**单帖**的点赞态有专门接口 `GET /api/v1/posts/:id/like-status`,**保持不变**。**但**:

- 详情页 SC 现已并发拉 `posts/:id` + `posts/:id/like-status`,2 次 RTT
- 若 `posts/:id` 响应**也**加 `liked_by_me`,前端可省一次 RTT。**非紧急**,Phase 2 性能收尾时再说。本项**只**要求 FeedResp 加,详情页可独立后做

### 2.6 前端落地动作(后端接口上线后)

1. `src/features/recipe/schema.ts` 的 `postBriefSchema` 加 `liked_by_me: z.boolean().optional()`(对齐 D-007 `is_following` 模式)
2. `src/types/domain.ts` 的 `PostBrief` 加 `liked_by_me?: boolean`
3. 卡片组件(PostCard / UserPostList 渲染处 / SearchResultList)把 `post.liked_by_me ?? false` 透传到 `LikeButton.initialLiked`,**替换**当前的 `false` 字面量
4. `DEBT_LEDGER` D-007 备注栏追加"§2 liked_by_me 已解",或新开 D-009 条目独立追踪

前端预计改动:≤ 15 行代码(3 处卡片渲染点 + schema + types)。

### 2.7 是否必须?

**不必须**,但**用户体感优先级 ≥ §1**。点赞是 Feed 上最高频交互,"心一直灰"对用户的"我点过赞"心智冲击比"关注按钮没变"更明显。建议 §2 优先于 §1 ship。

---

## 3. 历史 follow-up(已解决)

无。`backend-changes-pre-phase-1.md` 全部 🔴 项均已在 Phase 1 dev 阶段联调通过。

---

## 文档维护

- 后端新增 `is_following` 字段后:**双方**各自更新——前端改 D-007 状态 + page.tsx,后端在 `docs/api-spec/01-user.md`(后端 owner 同步) + commit message 引用本文档
- 后端新增 `liked_by_me` 字段后:前端改 PostBrief schema + types + 3 处卡片透传,后端在 `docs/api-spec/02-post.md` `FeedResp` 表里加该字段说明
- 后续若发现新 follow-up:在本文档加章节,不开新文档
