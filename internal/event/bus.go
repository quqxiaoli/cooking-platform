package event

import "context"

// Handler 是消费者处理函数的类型定义。
//
// 参数 payload 是 Event.Payload 的原始 JSON 字节，Consumer 自行 json.Unmarshal。
// 返回 nil 表示消费成功（对应 RabbitMQ 的 ACK）。
// 返回 error 表示消费失败（对应 RabbitMQ 的 Nack + 死信）。
//
// ── 失败语义对照表（Step 18 A7 / DELIVERY-01） ─────────────────────────────
//
// 这张表是 Channel 与 RabbitMQ 两种 Bus 实现行为差异的唯一权威。Consumer
// 编写 Handler 时**必须**按此契约决定何时返回 error、何时返回 nil。
//
//	          | Channel 实现                  | RabbitMQ 实现
//	──────────┼───────────────────────────────┼──────────────────────────────
//	Handler   | 仅 zap.Warn 记录失败，消息丢弃 | Nack(requeue=false) → DLX
//	返回 err  | 不重试，channel 内消息不重投   | "cooking.events.dlx.queue"
//	──────────┼───────────────────────────────┼──────────────────────────────
//	Handler   | Ack（隐式：channel 读完即丢）  | Ack(false) → broker 删除消息
//	返回 nil  |                               |
//	──────────┼───────────────────────────────┼──────────────────────────────
//	进程崩溃  | 队列内消息全部丢失             | 未 Ack 的消息会被重投
//	(handler |                               | (at-least-once)
//	 跑完之前)|                               |
//	──────────┼───────────────────────────────┼──────────────────────────────
//	业务编写  | 不要依赖 "返回 error 会重试" — Channel 不重试；
//	约束     | 同时也不要依赖 "处理过的事件不会再来" — RabbitMQ 至少一次。
//	         | 所有 Handler 必须幂等（INSERT IGNORE / GREATEST / dedup 缓存）。
//
// 已知偏离（按规则保留）：unmarshal 失败时两边都只 Warn 然后 return nil，
// 不让一条永远解析不出来的毒消息卡死 DLX —— 这是 PRD v3.0 §10 已签注的
// 例外，写在 channel.go / rabbitmq.go handleDelivery 注释里。
type Handler func(ctx context.Context, payload []byte) error

// EventPublisher 发布事件的接口。
//
// 业务 Service 层只依赖此接口，不感知底层是 Channel 还是 RabbitMQ。
// Publish 必须是非阻塞的：Channel 实现下若 channel 满则丢弃并打 warn 日志；
// RabbitMQ 实现下若连接断开则触发 fallback。
type EventPublisher interface {
	Publish(ctx context.Context, evt Event) error
}

// EventSubscriber 订阅事件的接口。
//
// Subscribe 是阻塞调用：在 ctx 被 cancel 之前持续消费。
// Consumer 在独立 goroutine 中调用 Subscribe，ConsumerManager 负责 goroutine 管理。
// 同一 topic 可以注册多个 Handler（多订阅者），每条消息广播给所有 Handler。
type EventSubscriber interface {
	Subscribe(ctx context.Context, topic string, handler Handler) error
}

// EventBus 组合发布与订阅能力，并提供优雅关闭方法。
//
// Close 必须：
//  1. 停止接收新的 Publish 调用（后续 Publish 返回 error）
//  2. 排空已在 channel 中的消息（Channel 实现）或关闭 MQ 连接（RabbitMQ 实现）
//  3. 等待所有 Handler goroutine 退出后返回
//
// 调用方（main.go）在 ConsumerManager.Shutdown() 完成后再调用 Close，
// 保证 Consumer 先退出、EventBus 后关闭，不出现"Consumer 还在读、Bus 已关"的竞态。
type EventBus interface {
	EventPublisher
	EventSubscriber
	Close() error
}
