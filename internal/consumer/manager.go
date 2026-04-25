// internal/consumer/manager.go

package consumer

import (
	"context"
	"sync"

	"go.uber.org/zap"
)

// ConsumerManager 管理所有业务 Consumer 的生命周期。
//
// 职责：
//   - Register：注册 Consumer，必须在 StartAll 之前调用
//   - StartAll：为每个 Consumer 启动一个独立 goroutine，阻塞消费直到 ctx cancel
//   - Shutdown：cancel ctx → 等待所有 Consumer goroutine 退出
//
// 与 EventBus 的关系：
//   - ConsumerManager 先 Shutdown（等 Consumer 退出）
//   - 然后 main.go 再调用 bus.Close()
//   - 保证"Consumer 还在读 channel"和"Bus 已关闭"不会并发出现
type ConsumerManager struct {
	consumers []EventConsumer
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewManager 创建一个空的 ConsumerManager。
func NewManager() *ConsumerManager {
	return &ConsumerManager{}
}

// Register 注册一个 Consumer。必须在 StartAll 之前调用。
func (m *ConsumerManager) Register(c EventConsumer) {
	m.consumers = append(m.consumers, c)
}

// StartAll 为每个已注册的 Consumer 启动一个 goroutine。
//
// 内部创建一个可 cancel 的 ctx，Shutdown 时调用 cancel 通知所有 Consumer 退出。
// Consumer 的 Start 返回后（无论正常还是 error），goroutine 退出并调用 wg.Done()。
func (m *ConsumerManager) StartAll() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	for _, c := range m.consumers {
		m.wg.Add(1)
		go func(c EventConsumer) {
			defer m.wg.Done()
			zap.L().Info("consumer started", zap.String("consumer", c.Name()))
			if err := c.Start(ctx); err != nil {
				zap.L().Error("consumer exited with error",
					zap.String("consumer", c.Name()),
					zap.Error(err),
				)
				return
			}
			zap.L().Info("consumer stopped", zap.String("consumer", c.Name()))
		}(c)
	}

	zap.L().Info("consumer manager started", zap.Int("consumers", len(m.consumers)))
}

// Shutdown 优雅关闭所有 Consumer。
//
// 流程：
//  1. cancel ctx → 通知所有 Consumer 的 Subscribe/Start 停止接收新消息
//  2. wg.Wait()  → 等待每个 Consumer 处理完当前消息后退出
//
// main.go 在 Shutdown 返回后再调用 bus.Close()，保证时序正确。
func (m *ConsumerManager) Shutdown() {
	if m.cancel == nil {
		// StartAll 从未被调用，直接返回
		return
	}
	zap.L().Info("consumer manager shutting down...")
	m.cancel()
	m.wg.Wait()
	zap.L().Info("consumer manager stopped")
}
