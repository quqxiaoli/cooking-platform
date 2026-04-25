package consumer

import "context"

// EventConsumer 是所有业务 Consumer 必须实现的接口。
//
// Name()  返回 Consumer 的唯一名称，用于日志标识和优雅关闭时的进度打印。
// Start() 是阻塞调用：在 ctx 被 cancel 之前持续消费事件。
//
//	ctx cancel 后，Start 应完成当前正在处理的消息，然后返回 nil。
//	若初始化失败（如订阅 topic 失败），返回 error，ConsumerManager 会记录日志。
//
// 实现约定：
//   - 在 Start 内部调用 bus.Subscribe(ctx, topic, handler)
//   - handler 处理完一条消息后返回，Subscribe 负责循环取下一条
//   - 不在 Start 内部启动额外 goroutine（goroutine 管理交给 ConsumerManager）
type EventConsumer interface {
	Name() string
	Start(ctx context.Context) error
}
