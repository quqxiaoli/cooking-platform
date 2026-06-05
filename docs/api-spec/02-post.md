# 02 · 内容模块（帖子）

> 代码来源：`internal/handler/post.go`、`internal/handler/feed.go`、`internal/service/post_service.go`、`internal/model/dto/post.go`、`internal/model/scene_tag.go`。

---

## 0. 场景标签字典（八固定值）

`model.SceneTag`（`internal/model/scene_tag.go:38`）。`scene_tag` 字段在所有写入 / 列表 / 详情接口里都用同一组整数 → 中文名映射：

| `scene_tag` | `scene_name` | 含义 |
| --- | --- | --- |
| 1 | 出租屋 | 单人 / 合租场景 |
| 2 | 一个人的饭 | 独居一餐 |
| 3 | 露营野炊 | 户外烹饪 |
| 4 | 家庭厨房 | 家庭日常 |
| 5 | 快手日常 | 15 分钟内做完 |
| 6 | 打包便当 | 带饭 / 备餐 |
| 7 | 减脂餐 | 控制卡路里 |
| 8 | 节气节日 | 节令食物 |

> 取值范围 `[1, 8]`。`0` 永久保留为非法占位（`SceneTagUnknown`），永远不会出现在响应里。**前端禁止依赖中文名做条件判断**，永远用 `scene_tag` 数值；`scene_name` 仅用于展示。

---

## 1. POST `/api/v1/posts` —— 发布帖子

**鉴权**：**必须**（+ 每用户 20 篇 / 24h 频控，命中报 `429001`）

**Request Body**（`dto.CreatePostReq` @ `dto/post.go:60`）

| 字段 | 类型 | 必填 | 校验 |
| --- | --- | --- | --- |
| `title` | string | 是 | `required,min=1,max=100`，中文按字符（非字节）计 |
| `scene_tag` | int8 | 是 | `required,min=1,max=8`（见上表） |
| `content` | string | 否 | `max=5000`。当 `steps` 为空时即为纯文本正文；非空时作为摘要 |
| `cover_url` | string | 否 | `omitempty,max=500`。**必须**是 OSS 白名单前缀（`oss.url_prefix`） |
| `steps` | array | 否 | `omitempty,max=30,dive`。每项见下 |

**`steps[]` 元素**（`dto.PostStepReq` @ `dto/post.go:74`）

| 字段 | 类型 | 必填 | 校验 |
| --- | --- | --- | --- |
| `text` | string | 是 | `required,max=500`。**注意**：即使有图也必须给文字 |
| `image_urls` | string[] | 否 | `omitempty,max=3,dive,max=500`。每个 URL 必须命中 OSS 白名单 |

**Response 成功**（`dto.CreatePostResp` @ `dto/post.go:94`）

```json
{
  "code": 0,
  "msg": "ok",
  "data": {
    "post_id": 1001,
    "audit_status": 0,
    "is_visible": 0,
    "created_at": 1714200000000
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `post_id` | 新帖子主键 |
| `audit_status` | **始终为 `0`（机审待处理）**。审核结果由 AuditConsumer 异步写回 |
| `is_visible` | **始终为 `0`（不可见）**，等审核通过（`audit_status` ∈ `{1, 3}`）后才会变 1。客户端不要假定刚发的帖马上能在 Feed 里看到 |
| `created_at` | UnixMilli |

> **关键**：发布即返回 200，但帖子 **此刻不会出现在 Feed / 搜索 / 作者列表**。dev 环境 `audit.provider=mock` 一般 1-3s 内通过；prod `audit.provider=aliyun` 异步调用，典型 5-10s 通过。客户端发完后**轮询 GET 详情**即可拿到最终 `is_visible`。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | JSON 体格式错 / 基础校验失败 |
| 412101 | 400 | title trim 后为空 |
| 412102 | 400 | title 超过 100 字 |
| 412103 | 400 | scene_tag ∉ [1, 8] |
| 412108 | 400 | content 超过 5000 字 |
| 460105 | 400 | cover_url 或任意 step 的 image_url 不在 OSS 白名单 |
| 460106 | 400 | steps 数组结构非法（数量超 30 / step.text 为空 / image_urls 超 3） |
| 401001 / 401002 / 401003 | 401 | 鉴权失败 |
| 429001 | 429 | 24h 内已发 20 篇 |
| 500001 | 500 | DB / 事务失败 |

**cURL**

```bash
curl -i -X POST https://mellowck.com/api/v1/posts \
  -H 'Authorization: Bearer <ACCESS_TOKEN>' \
  -H 'Content-Type: application/json' \
  -d '{
    "title": "出租屋一锅炖",
    "scene_tag": 1,
    "content": "懒人版番茄牛腩",
    "cover_url": "https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/cover/1001/202605/abc.jpg",
    "steps": [
      { "text": "牛腩冷水下锅焯水", "image_urls": [] },
      { "text": "番茄切块翻炒", "image_urls": ["https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/step/1001/202605/xyz.jpg"] }
    ]
  }'
```

---

## 2. GET `/api/v1/posts/:id` —— 帖子详情

**鉴权**：无（公开）

**Path Param**：`id` 帖子主键，正整数。

**Response 成功**（`dto.PostDetailResp` @ `dto/post.go:139`）

```json
{
  "code": 0,
  "data": {
    "id": 1001,
    "title": "出租屋一锅炖",
    "scene_tag": 1,
    "scene_name": "出租屋",
    "content": "懒人版番茄牛腩",
    "cover_url": "https://cooking-platform-prod.oss-cn-hongkong.aliyuncs.com/cover/1001/202605/abc.jpg",
    "like_count": 12,
    "view_count": 200,
    "author": {
      "id": 1001,
      "nickname": "厨友8000",
      "avatar_url": ""
    },
    "steps": [
      { "step_no": 1, "text": "牛腩冷水下锅焯水", "image_urls": [] },
      { "step_no": 2, "text": "番茄切块翻炒", "image_urls": ["https://..."] }
    ],
    "audit_status": 1,
    "is_visible": 1,
    "created_at": 1714200000000,
    "updated_at": 1714200500000
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `steps[]` | **空数组 = 旧式纯文本帖**，前端 fallback 渲染 `content` 即可；非空时建议按 `step_no` 顺序渲染（已升序） |
| `audit_status` | `0` 待审 / `1` 机审通过 / `2` 疑似（人工复审中） / `3` 人工通过 / `4` 机审拒 / `5` 人工拒。前端**只在自己看自己的帖子时**才需要展示此字段，公开 Feed 看到的永远是 `1` 或 `3` |
| `is_visible` | 公开访问能拿到的恒为 `1`；非公开会直接 404 |
| `view_count` | 调用本接口本身就会异步 +1（去重 1h，见 `cache.pv_dedup_ttl`） |

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | id 非数字或 ≤ 0 |
| 412104 | 404 | 帖子不存在 / is_visible=0（不区分） |

> **注意**：作者看自己被审核拒的帖也会返回 404。MVP 不区分"作者本人查看草稿"场景（PRD 偏离点已记录到 99-prd-deltas.md）。

---

## 3. GET `/api/v1/feed` —— 全局 / 场景 Feed

**鉴权**：可选（`OptionalAuth`）。带有效 token 时每个 `posts[].liked_by_me` 反映"我是否已点赞此帖"；匿名 / 无效 token → 全部 `false`。

**Query**（`dto.FeedQuery` @ `dto/post.go:85`）

| 字段 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `scene_tag` | int8 | 0 | 不传 / 传 0 = 全部场景；`1..8` = 单场景过滤 |
| `cursor` | string | "" | 首页空，后续页传上次的 `next_cursor` |
| `size` | int | 20 | `[1, 50]` |

**Response 成功**（`dto.FeedResp` @ `dto/post.go:157`）

```json
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
        "liked_by_me": true
      }
    ],
    "next_cursor": "1714199000000",
    "has_more": true
  }
}
```

| 字段 | 说明 |
| --- | --- |
| `posts` | 仅 `is_visible=1` 的帖子；列表项不含 `content` / `steps`（Feed 卡片只渲染标题 + 封面） |
| `next_cursor` | 不透明字符串，当前内部用 `created_at` UnixMilli。客户端**禁止解析** |
| `has_more` | 与 `next_cursor != ""` 等价。最后一页两者同为 `false` / `""` |
| `liked_by_me` | 当前 token 用户是否已点赞此帖。实现：Redis pipeline `SISMEMBER like:set:{post_id} <user_id>`，O(N) 一次 RTT。匿名 / Redis 失败 → `false`（不会 500）。Feed 缓存里 `liked_by_me` 恒为 `false`，按页解码后再做一次 batch 富集，**保证缓存可跨用户共享**。冷帖（`like:set:*` 已过 7d TTL）也会返 `false`，与 `GET /posts/:id/like-status` 的已知 gap 一致 |

**排序**：严格按 `created_at DESC, id DESC`（`internal/service/post_service.go:459`），同毫秒由 id 决定。

**Response 失败**

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | scene_tag 越界（非 0-8） |
| 412106 | 400 | cursor 解析失败（非纯数字） |
| 412107 | 400 | size 超出 [1, 50] |

**cURL**

```bash
# 首页（全部场景）
curl -i 'https://mellowck.com/api/v1/feed?size=20'

# 出租屋场景
curl -i 'https://mellowck.com/api/v1/feed?scene_tag=1&size=20'

# 下一页
curl -i 'https://mellowck.com/api/v1/feed?scene_tag=1&cursor=1714199000000&size=20'
```

---

## 4. 不存在的接口

| 路径 | 状态 | 说明 |
| --- | --- | --- |
| `PATCH /api/v1/posts/:id` | **不存在** | MVP 不支持编辑帖子 |
| `DELETE /api/v1/posts/:id` | **不存在** | MVP 不支持删除帖子 |
| `POST /api/v1/posts/:id/comment` | **不存在** | 评论功能产品决策永久排除（CLAUDE.md §4.5） |
| `GET /api/v1/posts/draft` | **不存在** | MVP 无草稿箱 |
