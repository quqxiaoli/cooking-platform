// Package event — rabbitmq.go implements EventBus backed by RabbitMQ
// (AMQP 0-9-1) using github.com/rabbitmq/amqp091-go.
//
// ── Wire topology ────────────────────────────────────────────────────────────
//
//	Main exchange:  "cooking.events",     type=topic,   durable=true
//	DLX exchange:   "cooking.events.dlx", type=fanout,  durable=true
//	DLX queue:      "cooking.events.dlx.queue"          catch-all for inspection
//	Queues:         "cooking.<topic>",    durable=true, exclusive=false
//	Routing key:    == topic string (e.g. "event.like")
//
// Multiple app instances compete on the same named queue per topic, achieving
// load-balanced consumption without a coordinator.
//
// ── Delivery semantics ───────────────────────────────────────────────────────
//
// DeliveryMode=Persistent: messages survive broker restart.
// autoAck=false: the handler must succeed before the message is ACKed.
//
//	Success → Ack(false)
//	Unmarshal / handler error → Nack(false, requeue=false) → DLX
//
// at-least-once: if the process crashes after handler success but before Ack,
// the broker redelivers on reconnect. All consumers are idempotent
// (INSERT IGNORE, GREATEST(0,…) on counts) so redelivery is safe.
//
// ── Dead-letter routing ──────────────────────────────────────────────────────
//
// Any Nack(requeue=false) is routed to "cooking.events.dlx" (fanout) and
// lands in "cooking.events.dlx.queue" for inspection and alerting.
// This prevents poison-message loops without requiring quorum queues.
//
// ── Reconnect ────────────────────────────────────────────────────────────────
//
// Publish: on channel/connection failure, lazy reconnect with exponential
// backoff (ReconnectInitialDelay × 2^(attempt-1), capped at 30s), up to
// ReconnectMaxRetries attempts.
//
// Subscribe: on msgs-channel closure (connection lost), backs off then
// re-dials via ensureConnLocked — only one goroutine actually re-dials even
// when multiple Subscribe goroutines reconnect concurrently.
//
// ── Concurrency ──────────────────────────────────────────────────────────────
//
// b.mu protects b.conn and b.pubCh. Subscribe callers open their own AMQP
// channel per goroutine (under the mutex for the open call only) and then
// operate independently — no shared state in the subscribe hot path.
//
// ── Close semantics ──────────────────────────────────────────────────────────
//
// Close() closes the AMQP connection. All blocked Subscribe calls receive a
// channel-closed signal and return nil. ConsumerManager calls Close() only
// after all consumers have exited, preventing "consumer still reading, bus
// already closed" races.
package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"cooking-platform/pkg/config"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

const (
	rabbitMQExchange    = "cooking.events"
	rabbitMQDLXExchange = "cooking.events.dlx"
	rabbitMQDLXQueue    = "cooking.events.dlx.queue"
)

// RabbitMQBus implements EventBus using AMQP 0-9-1.
type RabbitMQBus struct {
	cfg    config.MQConfig
	mu     sync.Mutex
	conn   *amqp.Connection
	pubCh  *amqp.Channel
	closed atomic.Bool
}

// NewRabbitMQBus dials the AMQP broker, declares the exchange topology, and
// opens a shared publish channel. Returns error if the initial dial fails.
func NewRabbitMQBus(cfg config.MQConfig) (*RabbitMQBus, error) {
	b := &RabbitMQBus{cfg: cfg}
	if err := b.dialAndDeclareLocked(); err != nil {
		return nil, err
	}
	zap.L().Info("event bus initialized", zap.String("provider", "rabbitmq"))
	return b, nil
}

// dialAndDeclareLocked establishes a fresh AMQP connection, opens the publish
// channel, and declares the exchange topology. Replaces the existing conn/pubCh
// when called during reconnect. Callers must hold b.mu OR be single-threaded
// (as in NewRabbitMQBus before any goroutines are started).
func (b *RabbitMQBus) dialAndDeclareLocked() error {
	conn, err := amqp.DialConfig(b.cfg.URL, amqp.Config{
		Dial: amqp.DefaultDial(b.cfg.Timeout),
	})
	if err != nil {
		return fmt.Errorf("rabbitmq dial: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("rabbitmq open publish channel: %w", err)
	}

	if err := declareTopology(ch); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return err
	}

	// Release previous resources if this is a reconnect.
	if b.conn != nil {
		_ = b.pubCh.Close()
		_ = b.conn.Close()
	}
	b.conn = conn
	b.pubCh = ch
	return nil
}

// ensureConnLocked re-dials only when the current connection is closed.
// No-ops if the connection is still healthy. Callers must hold b.mu.
func (b *RabbitMQBus) ensureConnLocked() error {
	if b.conn != nil && !b.conn.IsClosed() {
		return nil
	}
	return b.dialAndDeclareLocked()
}

// reopenPubChLocked closes the current publish channel and opens a fresh one.
// Falls back to a full redial if the connection itself is dead.
// Callers must hold b.mu.
func (b *RabbitMQBus) reopenPubChLocked() error {
	_ = b.pubCh.Close()
	if b.conn == nil || b.conn.IsClosed() {
		// dialAndDeclareLocked creates both a new conn and a new pubCh.
		return b.dialAndDeclareLocked()
	}
	ch, err := b.conn.Channel()
	if err != nil {
		// Channel open failed despite conn appearing alive — try full redial.
		return b.dialAndDeclareLocked()
	}
	b.pubCh = ch
	return nil
}

// declareTopology idempotently declares the main exchange, DLX exchange, and
// the DLX catch-all queue. Safe to call on every reconnect.
func declareTopology(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(
		rabbitMQExchange, "topic",
		true, false, false, false, nil,
	); err != nil {
		return fmt.Errorf("rabbitmq declare exchange: %w", err)
	}

	// DLX exchange: fanout so dead letters land in the catch-all queue
	// regardless of the original routing key.
	if err := ch.ExchangeDeclare(
		rabbitMQDLXExchange, "fanout",
		true, false, false, false, nil,
	); err != nil {
		return fmt.Errorf("rabbitmq declare DLX exchange: %w", err)
	}

	// Catch-all DLX queue for dead-letter inspection and alerting.
	if _, err := ch.QueueDeclare(
		rabbitMQDLXQueue,
		true, false, false, false, nil,
	); err != nil {
		return fmt.Errorf("rabbitmq declare DLX queue: %w", err)
	}
	if err := ch.QueueBind(rabbitMQDLXQueue, "", rabbitMQDLXExchange, false, nil); err != nil {
		return fmt.Errorf("rabbitmq bind DLX queue: %w", err)
	}

	return nil
}

// queueName returns the shared durable queue name for a topic.
// Convention: "cooking.<topic>" (e.g. "cooking.event.like").
// All app instances compete on the same queue → load-balanced consumption.
func queueName(topic string) string {
	return "cooking." + topic
}

// reconnectDelay returns the exponential backoff duration for attempt n (1-indexed).
// Formula: ReconnectInitialDelay × 2^(n-1), capped at 30s.
func (b *RabbitMQBus) reconnectDelay(attempt int) time.Duration {
	d := float64(b.cfg.ReconnectInitialDelay) * math.Pow(2, float64(attempt-1))
	const maxDelay = float64(30 * time.Second)
	if d > maxDelay {
		d = maxDelay
	}
	return time.Duration(d)
}

// Publish serialises evt and publishes with DeliveryMode=Persistent.
// On channel/connection failure, retries up to ReconnectMaxRetries times
// with exponential backoff.
func (b *RabbitMQBus) Publish(ctx context.Context, evt Event) error {
	if b.closed.Load() {
		return errors.New("rabbitmq bus: publish on closed bus")
	}

	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("rabbitmq bus: marshal event: %w", err)
	}

	msg := amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Body:         body,
	}

	for attempt := 0; attempt <= b.cfg.ReconnectMaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(b.reconnectDelay(attempt)):
			case <-ctx.Done():
				return ctx.Err()
			}
			if b.closed.Load() {
				return errors.New("rabbitmq bus: closed during publish retry")
			}

			b.mu.Lock()
			reconnErr := b.reopenPubChLocked()
			b.mu.Unlock()
			if reconnErr != nil {
				zap.L().Warn("rabbitmq: publish reconnect failed",
					zap.Int("attempt", attempt), zap.Error(reconnErr))
				continue
			}
		}

		b.mu.Lock()
		pubErr := b.pubCh.PublishWithContext(ctx, rabbitMQExchange, evt.Topic, false, false, msg)
		b.mu.Unlock()

		if pubErr == nil {
			return nil
		}
		if ctx.Err() != nil || b.closed.Load() {
			return pubErr
		}
		zap.L().Warn("rabbitmq: publish failed, will retry",
			zap.Int("attempt", attempt),
			zap.String("topic", evt.Topic),
			zap.Error(pubErr),
		)
	}
	return fmt.Errorf("rabbitmq: publish max retries exceeded for topic %s", evt.Topic)
}

// Subscribe declares a named durable queue for topic, consumes with manual ACK,
// and reconnects on connection loss.
//
// Returns nil on clean ctx cancellation.
// Returns error only when ReconnectMaxRetries is exhausted.
func (b *RabbitMQBus) Subscribe(ctx context.Context, topic string, handler Handler) error {
	for attempt := 0; attempt <= b.cfg.ReconnectMaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(b.reconnectDelay(attempt)):
			case <-ctx.Done():
				return nil
			}

			b.mu.Lock()
			if err := b.ensureConnLocked(); err != nil {
				b.mu.Unlock()
				zap.L().Warn("rabbitmq: subscribe redial failed",
					zap.String("topic", topic),
					zap.Int("attempt", attempt),
					zap.Error(err),
				)
				continue
			}
			b.mu.Unlock()

			zap.L().Info("rabbitmq: subscribe reconnected",
				zap.String("topic", topic),
				zap.Int("attempt", attempt),
			)
		}

		connLost, err := b.subscribeOnce(ctx, topic, handler)
		if !connLost {
			return nil // clean shutdown via ctx cancellation
		}
		if ctx.Err() != nil {
			return nil
		}
		zap.L().Warn("rabbitmq: subscribe connection lost, will reconnect",
			zap.String("topic", topic),
			zap.Int("attempt", attempt),
			zap.Error(err),
		)
	}
	return fmt.Errorf("rabbitmq subscribe: max retries exceeded for topic %s", topic)
}

// subscribeOnce runs one subscribe session until ctx is cancelled or the
// connection is lost.
//
// Returns (connectionLost=false, nil) on clean ctx cancellation.
// Returns (connectionLost=true, err) when the msgs channel closes unexpectedly.
func (b *RabbitMQBus) subscribeOnce(ctx context.Context, topic string, handler Handler) (bool, error) {
	b.mu.Lock()
	subCh, err := b.conn.Channel()
	b.mu.Unlock()
	if err != nil {
		return true, fmt.Errorf("open sub channel: %w", err)
	}
	defer subCh.Close()

	qName := queueName(topic)
	if _, err := subCh.QueueDeclare(
		qName,
		true,  // durable: survives broker restart
		false, // autoDelete
		false, // exclusive=false: shared across all app instances
		false,
		amqp.Table{"x-dead-letter-exchange": rabbitMQDLXExchange},
	); err != nil {
		return true, fmt.Errorf("queue declare %q: %w", qName, err)
	}

	if err := subCh.QueueBind(qName, topic, rabbitMQExchange, false, nil); err != nil {
		return true, fmt.Errorf("queue bind %q: %w", qName, err)
	}

	msgs, err := subCh.Consume(
		qName,
		"",    // consumer tag: auto-generated per session
		false, // autoAck=false: manual ACK/Nack for at-least-once delivery
		false, // exclusive=false: multiple instances consume the same queue
		false, false, nil,
	)
	if err != nil {
		return true, fmt.Errorf("consume %q: %w", qName, err)
	}

	for {
		select {
		case delivery, ok := <-msgs:
			if !ok {
				return true, fmt.Errorf("msgs channel closed (connection lost)")
			}
			b.handleDelivery(ctx, topic, delivery, handler)

		case <-ctx.Done():
			// Drain buffered deliveries using a background ctx so handlers
			// complete normally even though the parent ctx is cancelled.
			for {
				select {
				case delivery, ok := <-msgs:
					if !ok {
						return false, nil
					}
					b.handleDelivery(context.Background(), topic, delivery, handler)
				default:
					return false, nil
				}
			}
		}
	}
}

// handleDelivery unmarshals a single AMQP delivery and invokes the handler.
// ACKs on success; Nacks without requeue (→ DLX) on unmarshal or handler error.
func (b *RabbitMQBus) handleDelivery(ctx context.Context, topic string, d amqp.Delivery, handler Handler) {
	var evt Event
	if err := json.Unmarshal(d.Body, &evt); err != nil {
		zap.L().Warn("rabbitmq: unmarshal failed",
			zap.String("topic", topic), zap.Error(err))
		_ = d.Nack(false, false) // bad payload → DLX
		return
	}
	if err := handler(ctx, evt.Payload); err != nil {
		zap.L().Warn("rabbitmq: handler error",
			zap.String("topic", topic), zap.Error(err))
		_ = d.Nack(false, false) // handler error → DLX; no infinite retry loop
		return
	}
	_ = d.Ack(false)
}

// Close closes the AMQP connection. All blocked Subscribe goroutines receive
// a channel-closed signal on their msgs channel and return nil. Idempotent.
func (b *RabbitMQBus) Close() error {
	if b.closed.Swap(true) {
		return nil
	}
	b.mu.Lock()
	_ = b.pubCh.Close()
	b.mu.Unlock()
	if err := b.conn.Close(); err != nil {
		return fmt.Errorf("rabbitmq connection close: %w", err)
	}
	zap.L().Info("event bus closed", zap.String("provider", "rabbitmq"))
	return nil
}
