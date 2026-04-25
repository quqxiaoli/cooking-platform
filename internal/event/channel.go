package event

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

// ChannelBus 是 EventBus 的进程内实现，基于 Go buffered channel。
//
// 适用阶段：MVP 开发阶段（零外部依赖，启动即用）。
// 局限性：进程重启消息全部丢失；不支持多实例消费（水平扩展时需切换 RabbitMQ）。
// 切换时机：第 13 步修改 config.yaml 中 mq.provider=rabbitmq，业务代码零修改。
type ChannelBus struct {
	mu      sync.RWMutex
	subs    map[string][]chan []byte // topic -> 该 topic 下所有订阅者的独立 channel
	bufSize int
	closed  atomic.Bool
}

// NewChannelBus 创建一个进程内 EventBus 实例。
//
// bufSize 是每个订阅者 channel 的缓冲大小。
// 推荐值：1024（约 200KB/topic，可吸收 ~1s 的 1000 events/s 突发）。
func NewChannelBus(bufSize int) *ChannelBus {
	return &ChannelBus{
		subs:    make(map[string][]chan []byte),
		bufSize: bufSize,
	}
}

// Publish 将事件发布到对应 topic 的所有订阅者 channel。
//
// 非阻塞：若某个订阅者的 channel 已满，丢弃该条消息并打 warn 日志，不影响其他订阅者。
// Bus 关闭后调用 Publish 直接返回 error，不 panic。
func (b *ChannelBus) Publish(ctx context.Context, evt Event) error {
	if b.closed.Load() {
		return errors.New("event bus: publish on closed bus")
	}

	// Payload 已经是 json.RawMessage（[]byte），直接发送，Consumer 按 topic 对应的结构体 Unmarshal。
	payload := []byte(evt.Payload)

	b.mu.RLock()
	channels := make([]chan []byte, len(b.subs[evt.Topic]))
	copy(channels, b.subs[evt.Topic])
	b.mu.RUnlock()

	for _, ch := range channels {
		select {
		case ch <- payload:
			// 正常投递
		default:
			// channel 满：丢弃 + warn，不阻塞业务线程。
			// 持续出现此日志说明 Consumer 处理能力不足，需排查或增加并发。
			zap.L().Warn("event bus channel full, message dropped",
				zap.String("topic", evt.Topic),
				zap.String("event_id", evt.ID),
			)
		}
	}
	return nil
}

// Subscribe 在当前 goroutine 中阻塞消费指定 topic 的消息，直到 ctx 被 cancel。
//
// ctx cancel 后，Subscribe 会先排空 channel 中剩余消息（用 context.Background() 处理，
// 避免 handler 内部判断 ctx.Err() 导致提前中止），排空完成后返回 nil。
//
// 同一 topic 可多次调用 Subscribe 注册不同 Handler，每条消息广播给所有 Handler。
// Handler 返回 error 时只打 warn 日志，不影响其他消息的处理（Channel 模式下不重试）。
func (b *ChannelBus) Subscribe(ctx context.Context, topic string, handler Handler) error {
	ch := make(chan []byte, b.bufSize)

	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()

	// 退出时从订阅列表中移除自己，避免 Publish 往已无人消费的 channel 写消息。
	defer func() {
		b.mu.Lock()
		channels := b.subs[topic]
		for i, c := range channels {
			if c == ch {
				b.subs[topic] = append(channels[:i], channels[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
	}()

	for {
		select {
		case payload, ok := <-ch:
			if !ok {
				// channel 被外部关闭（正常情况不会走到这里，Close 不关闭 channel）
				return nil
			}
			if err := handler(ctx, payload); err != nil {
				zap.L().Warn("event handler error",
					zap.String("topic", topic),
					zap.Error(err),
				)
			}

		case <-ctx.Done():
			// ctx 已 cancel（ConsumerManager 触发关闭），排空 channel 剩余消息后退出。
			// 用 context.Background() 而非已取消的 ctx，避免 handler 内部检查 ctx.Err() 时提前退出。
			b.drain(topic, ch, handler)
			return nil
		}
	}
}

// drain 在 ctx cancel 后排空 channel 中剩余的消息，确保已入队事件不丢失。
func (b *ChannelBus) drain(topic string, ch chan []byte, handler Handler) {
	for {
		select {
		case payload, ok := <-ch:
			if !ok {
				return
			}
			if err := handler(context.Background(), payload); err != nil {
				zap.L().Warn("event handler error during drain",
					zap.String("topic", topic),
					zap.Error(err),
				)
			}
		default:
			// channel 已空，排空完成
			return
		}
	}
}

// Close 标记 Bus 为关闭状态，后续 Publish 调用将返回 error。
//
// Close 不负责等待 Subscribe goroutine 退出，那是 ConsumerManager 的职责：
// ConsumerManager.Shutdown() → cancel ctx → Subscribe 排空退出 → wg.Wait() 完成
// → main.go 调用 bus.Close()
func (b *ChannelBus) Close() error {
	b.closed.Store(true)
	zap.L().Info("event bus closed", zap.String("provider", "channel"))
	return nil
}

// MustMarshalPayload 是业务 Service 层的辅助函数：将具体 Payload 结构体序列化为 json.RawMessage。
// 用法：
//
//	evt := Event{
//	    ID:        uuid.NewString(),
//	    Topic:     TopicLike,
//	    Timestamp: time.Now().UnixMilli(),
//	    Payload:   MustMarshalPayload(LikeEvent{...}),
//	}
//	publisher.Publish(ctx, evt)
func MustMarshalPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Payload 结构体均为简单类型，序列化失败只可能是编程错误，直接 panic 在启动时暴露
		panic("event: failed to marshal payload: " + err.Error())
	}
	return b
}
