// internal/event/types.go

package event

import "encoding/json"

// Topic 用字符串常量，不用 iota int。
// 原因：RabbitMQ routing key 本身是字符串；日志里可读；Channel map 直接用 string 做 key 无需转换。
const (
	TopicLike     = "event.like"
	TopicUnlike   = "event.unlike"
	TopicPV       = "event.pv"
	TopicAudit    = "event.audit"
	TopicCount    = "event.count"
	TopicPost        = "event.post"
	TopicPostDeleted = "event.post.deleted" // 删帖事件 → CountConsumer 把 users.post_count -1
	TopicFollow      = "event.follow"       // [Step 8] 关注事件 → CountConsumer 维护双向计数
	TopicUnfollow    = "event.unfollow"     // [Step 8] 取消关注事件 → CountConsumer 维护双向计数
)

// Event 是所有消息的信封结构。
// ID 用于幂等：Consumer 在处理前可以检查此 ID 是否已处理过。
// Payload 是 json.RawMessage，消费方按 Topic 对应的 XxxEvent 结构体 Unmarshal。
type Event struct {
	ID        string          `json:"id"`        // UUID v4，由 Publisher 在 Publish 时生成
	Topic     string          `json:"topic"`     // 与 TopicXxx 常量对应
	Timestamp int64           `json:"timestamp"` // UnixMilli，便于时序分析和消息超时判断
	Payload   json.RawMessage `json:"payload"`   // 具体事件 payload 的 JSON 序列化结果
}

// ─── Payload 结构体 ──────────────────────────────────────────────────────────

// LikeEvent 点赞事件，由 LikeService.Like() 发布到 TopicLike。
// AuthorID 是帖子作者 ID，Consumer 用它更新 users.total_likes 冗余字段。
type LikeEvent struct {
	EventID   string `json:"event_id"` // 与外层 Event.ID 一致，Consumer 层幂等用
	UserID    int64  `json:"user_id"`  // 点赞者
	PostID    int64  `json:"post_id"`
	AuthorID  int64  `json:"author_id"` // 帖子作者（用于冗余计数更新）
	Timestamp int64  `json:"timestamp"`
}

// UnlikeEvent 取消点赞事件，由 LikeService.Unlike() 发布到 TopicUnlike。
type UnlikeEvent struct {
	EventID   string `json:"event_id"`
	UserID    int64  `json:"user_id"`
	PostID    int64  `json:"post_id"`
	AuthorID  int64  `json:"author_id"`
	Timestamp int64  `json:"timestamp"`
}

// PVEvent 浏览量事件，由 PostService.GetDetail() 发布到 TopicPV。
// IP 在 Service 层脱敏后填入（如只保留前两段 x.x.*.*）。
// ViewerID=0 表示未登录访客。
type PVEvent struct {
	EventID   string `json:"event_id"`
	PostID    int64  `json:"post_id"`
	ViewerID  int64  `json:"viewer_id"` // 0 = 未登录
	IP        string `json:"ip"`        // 已脱敏
	Timestamp int64  `json:"timestamp"`
}

// AuditEvent 审核结果事件，由阿里云审核回调或人工审核接口发布到 TopicAudit。
// AuditStatus 对应 posts.audit_status 的枚举值（见 PRD-Phase3 §9）。
type AuditEvent struct {
	EventID     string `json:"event_id"`
	PostID      int64  `json:"post_id"`
	AuthorID    int64  `json:"author_id"`
	AuditStatus int8   `json:"audit_status"` // 1=机审通过 2=疑似 3=人工通过 4=拒绝 5=屏蔽
	Remark      string `json:"remark"`
	RawResponse string `json:"raw_response"` // 第三方 API 原始返回（仅用于 audit_log 存储）
	Timestamp   int64  `json:"timestamp"`
}

// CountEvent 冗余计数变更事件，由各 Consumer 在完成主操作后发布到 TopicCount。
// Delta 只允许 +1 或 -1。CountType 对应 users 表的列名，Consumer 用于拼 SQL。
type CountEvent struct {
	EventID   string `json:"event_id"`
	UserID    int64  `json:"user_id"`
	CountType string `json:"count_type"` // "post_count" | "total_likes" | "follower_count" | "following_count"
	Delta     int    `json:"delta"`      // +1 or -1
	Timestamp int64  `json:"timestamp"`
}

// PostEvent 发帖事件，由 PostService.Create() 发布到 TopicPost。
// 当前阶段触发：更新 users.post_count 冗余字段。
// 二期：触发 Feed 缓存失效、推送订阅者通知。
type PostEvent struct {
	EventID   string `json:"event_id"`
	PostID    int64  `json:"post_id"`
	AuthorID  int64  `json:"author_id"`
	SceneTag  int8   `json:"scene_tag"`
	Timestamp int64  `json:"timestamp"`
}

// PostDeletedEvent 删帖事件，由 PostService.Delete() 在成功软删 posts 行后
// 发布到 TopicPostDeleted。CountConsumer 消费时把 AuthorID 的 users.post_count
// 减 1，并用 GREATEST(0, ...) 钳位防 unsigned 列下溢（与 unlike/unfollow 同模式）。
type PostDeletedEvent struct {
	EventID   string `json:"event_id"`
	PostID    int64  `json:"post_id"`
	AuthorID  int64  `json:"author_id"`
	Timestamp int64  `json:"timestamp"`
}

// FollowEvent 关注事件，由 FollowService.Follow() 在成功写入 follows 表后
// 发布到 TopicFollow。
//
// 一条 FollowEvent 影响两个用户的冗余计数，CountConsumer 消费时拆成两笔：
//   - FollowerID  的 users.following_count +1（"我关注的人"数）
//   - FollowingID 的 users.follower_count  +1（"关注我的人"数）
//
// follows 表本身已由 FollowService 同步写入（真源），本事件仅驱动 users 表
// 上的冗余展示计数 —— 丢失一条只造成计数漂移，不影响关注关系本身。
type FollowEvent struct {
	EventID     string `json:"event_id"`     // 与外层 Event.ID 一致，Consumer 层幂等用
	FollowerID  int64  `json:"follower_id"`  // 关注发起者
	FollowingID int64  `json:"following_id"` // 被关注者
	Timestamp   int64  `json:"timestamp"`    // UnixMilli
}

// UnfollowEvent 取消关注事件，由 FollowService.Unfollow() 在成功删除 follows
// 行后发布到 TopicUnfollow。CountConsumer 消费时拆成两笔 -1：
//   - FollowerID  的 users.following_count -1
//   - FollowingID 的 users.follower_count  -1
//
// 两笔 -1 均由 CountConsumer 用 GREATEST(0, ...) 钳位，防 unsigned 列下溢
// （重复投递场景下的兜底，与 like/unlike 的 total_likes 处理一致）。
type UnfollowEvent struct {
	EventID     string `json:"event_id"`
	FollowerID  int64  `json:"follower_id"`
	FollowingID int64  `json:"following_id"`
	Timestamp   int64  `json:"timestamp"`
}
