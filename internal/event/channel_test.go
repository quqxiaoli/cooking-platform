// internal/event/channel_test.go

package event_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cooking-platform/internal/event"
)

// makeEvent 构造一个测试用 Event，payload 为简单 JSON 对象。
func makeEvent(topic string, id string) event.Event {
	payload, _ := json.Marshal(map[string]string{"msg": "hello", "id": id})
	return event.Event{
		ID:        id,
		Topic:     topic,
		Timestamp: time.Now().UnixMilli(),
		Payload:   payload,
	}
}

// TestChannelBus_PublishSubscribe 验证基本发布/订阅功能：
// 发布一条消息，订阅者能收到且 payload 内容正确。
func TestChannelBus_PublishSubscribe(t *testing.T) {
	bus := event.NewChannelBus(64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan []byte, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = bus.Subscribe(ctx, event.TopicLike, func(_ context.Context, payload []byte) error {
			received <- payload
			return nil
		})
	}()

	// 等订阅者 goroutine 启动并注册到 bus
	time.Sleep(10 * time.Millisecond)

	evt := makeEvent(event.TopicLike, "evt-001")
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("Publish error: %v", err)
	}

	select {
	case payload := <-received:
		var m map[string]string
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("unmarshal payload error: %v", err)
		}
		if m["id"] != "evt-001" {
			t.Errorf("expected id=evt-001, got %q", m["id"])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout: subscriber did not receive message")
	}

	cancel()
	wg.Wait()
	_ = bus.Close()
}

// TestChannelBus_MultipleSubscribers 验证多订阅者广播：
// 同一 topic 注册 3 个订阅者，发布 1 条消息，3 个都能收到。
func TestChannelBus_MultipleSubscribers(t *testing.T) {
	bus := event.NewChannelBus(64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const subCount = 3
	var receivedCount atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < subCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = bus.Subscribe(ctx, event.TopicPV, func(_ context.Context, _ []byte) error {
				receivedCount.Add(1)
				return nil
			})
		}()
	}

	// 等所有订阅者注册完成
	time.Sleep(20 * time.Millisecond)

	evt := makeEvent(event.TopicPV, "evt-pv-001")
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("Publish error: %v", err)
	}

	// 等消息被消费
	time.Sleep(50 * time.Millisecond)

	cancel()
	wg.Wait()
	_ = bus.Close()

	if got := receivedCount.Load(); got != subCount {
		t.Errorf("expected %d subscribers to receive message, got %d", subCount, got)
	}
}

// TestChannelBus_CloseStopsPublish 验证 Close 后不再接受新消息：
// Close 之后调用 Publish 应返回 error。
func TestChannelBus_CloseStopsPublish(t *testing.T) {
	bus := event.NewChannelBus(64)

	if err := bus.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	evt := makeEvent(event.TopicPost, "evt-post-001")
	err := bus.Publish(context.Background(), evt)
	if err == nil {
		t.Fatal("expected error after Close, got nil")
	}
}

// TestChannelBus_DrainOnCancel 验证优雅关闭时排空逻辑：
// channel 里已有 5 条消息，cancel ctx 后 Subscribe 应把 5 条都处理完再退出，不丢消息。
func TestChannelBus_DrainOnCancel(t *testing.T) {
	bus := event.NewChannelBus(64)

	ctx, cancel := context.WithCancel(context.Background())

	var processedCount atomic.Int32

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = bus.Subscribe(ctx, event.TopicCount, func(_ context.Context, _ []byte) error {
			// 模拟处理耗时，确保消息在 cancel 前已入队但还未全部消费
			time.Sleep(5 * time.Millisecond)
			processedCount.Add(1)
			return nil
		})
	}()

	// 等订阅者注册
	time.Sleep(10 * time.Millisecond)

	// 发布 5 条消息入队
	const msgCount = 5
	for i := 0; i < msgCount; i++ {
		evt := makeEvent(event.TopicCount, "evt-count-drain")
		if err := bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("Publish[%d] error: %v", i, err)
		}
	}

	// 立即 cancel，此时部分消息可能还在 channel 里
	cancel()
	wg.Wait()
	_ = bus.Close()

	if got := processedCount.Load(); got != msgCount {
		t.Errorf("expected all %d messages processed after drain, got %d", msgCount, got)
	}
}
