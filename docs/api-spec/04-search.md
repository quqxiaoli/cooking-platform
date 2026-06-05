# 04 · 搜索模块

> 代码来源：`internal/handler/search.go`、`internal/service/search_service.go`、`internal/repository/search_repository.go`、`internal/model/dto/search.go`。

---

## 1. GET `/api/v1/search` —— 全文搜索帖子

**鉴权**：可选（`OptionalAuth`，公开）。带有效 token 时每个 `posts[].liked_by_me` 反映当前用户的点赞态；匿名 / 无效 token → 全部 `false`。实现细节见 [02-post.md §3](./02-post.md) 的 `liked_by_me` 段。

**频控**：**每 IP 30 次 / 60s**（`cmd/server/main.go:485`，命中报 `429001`）。nginx 边缘还有一层 per-IP，命中也是 429——前端看到 429 一律按"放慢"处理即可，不必区分。

**Query**（`dto.SearchQuery` @ `dto/search.go:46`）

| 字段 | 类型 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- | --- |
| `q` | string | 是 | — | 关键词。**注意 query 名是 `q` 不是 `keyword`** |
| `scene_tag` | int8 | 否 | 0 = 全部 | `[1, 8]`，见 [02-post.md §0](./02-post.md) |
| `cursor` | string | 否 | "" | 不透明分页游标，首页传空 |
| `size` | int | 否 | 20 | `[1, 50]` |

### 1.1 关键词处理规则（`search_service.normaliseKeyword`）

| 输入 | 处理 |
| --- | --- |
| 空 / 仅空白 | 直接报 `450101 ErrSearchKeywordEmpty` |
| 超过 `search.max_keyword_len`（prod `50` 字符）| **截断**到 50 字（**按 rune 计算，不是 byte**），不报错 |
| 含 MySQL Boolean Mode 保留符 `+-><()~*"@` | 全部剥离（`config.search.boolean_operators`） |
| 处理后仍非空 | 进入 BOOLEAN 模式全文检索 `posts.title + content` |
| 处理后变空（输入只是符号） | 报 `450101 ErrSearchKeywordEmpty` |

> 中文按 ngram parser 分词（MySQL 8.0 默认 `ngram_token_size=2`，详见 migrations `000002_create_posts_table.up.sql` 的 FULLTEXT 索引定义）。所以单字搜索（如 `q=肉`）**会**返回结果。

### 1.2 排序与分页

- 排序：MySQL FULLTEXT BOOLEAN MODE 默认按相关度（无法自定义）。
- 分页：用 **OFFSET 数字**而不是时间戳（`internal/service/search_service.go:209`）。`cursor` 是十进制 OFFSET 字符串，**客户端禁止解析**。
- 性能：深翻页（OFFSET > 1000）会逐渐变慢，前端不要做"跳页"UI，只做"滚动加载下一页"。

### 1.3 Response 成功（`dto.SearchResp` @ `dto/search.go:63`）

```json
{
  "code": 0,
  "data": {
    "posts": [
      {
        "id": 1001,
        "title": "番茄牛腩 出租屋一锅炖",
        "scene_tag": 1,
        "scene_name": "出租屋",
        "cover_url": "https://...",
        "like_count": 12,
        "view_count": 200,
        "author": { "id": 1001, "nickname": "厨友8000", "avatar_url": "" },
        "created_at": 1714200000000,
        "liked_by_me": false
      }
    ],
    "next_cursor": "20",
    "has_more": true
  }
}
```

- `posts` 复用 `dto.PostListItem`——与 Feed 列表项**结构完全一致**，前端可以共享卡片组件。
- `posts` 仅含 `is_visible=1` 的帖子。
- 列表项**不带** `content` / `steps`。
- `next_cursor == ""` ⇔ `has_more == false`：最后一页。

### 1.4 Response 失败

| code | HTTP | 触发条件 |
| --- | --- | --- |
| 400001 | 400 | scene_tag / size 校验失败 |
| 412107 | 400 | size 超 [1, 50] |
| 450101 | 400 | 关键词为空 / 仅空白 / 仅保留符 |
| 450102 | 400 | cursor 解析失败（非纯数字） |
| 429001 | 429 | 同一 IP 60s 内 ≥ 30 次 |
| 500001 | 500 | DB / 索引失败 |

### 1.5 cURL

```bash
# 基础搜索
curl -i 'https://mellowck.com/api/v1/search?q=番茄牛腩'

# 按场景过滤 + 翻页
curl -i 'https://mellowck.com/api/v1/search?q=牛腩&scene_tag=1&size=10'
curl -i 'https://mellowck.com/api/v1/search?q=牛腩&scene_tag=1&size=10&cursor=10'
```

---

## 2. 不存在的接口

| 路径 | 状态 | 说明 |
| --- | --- | --- |
| `GET /api/v1/search/users` | **不存在** | MVP 不支持搜用户 |
| `GET /api/v1/search/suggest` | **不存在** | MVP 不提供搜索联想 |
| `GET /api/v1/search/hot` | **不存在** | MVP 不提供热搜词 |
