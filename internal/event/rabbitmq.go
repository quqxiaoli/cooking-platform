// Package event — rabbitmq.go implements EventBus backed by RabbitMQ
// (AMQP 0-9-1) using github.com/rabbitmq/amqp091-go.
//
// ── Wire topology ────────────────────────────────────────────────────────────
//
//	Exchange: "cooking.events", type=topic, durable=true
//	Queues:   server-generated name, ephemeral, auto-delete, exclusive
//	Routing:  routing key == topic string (e.g. "event.like")
//
// Each Subscribe call opens its own AMQP channel + queue, mirroring
// ChannelBus's per-subscriber goroutine model. Consumers need zero changes
// when switching from ChannelBus to RabbitMQBus.
//
// ── Delivery semantics ───────────────────────────────────────────────────────
//
// autoAck=true (at-most-once) for MVP — matches ChannelBus. Step 13
// production hardening will flip to manual ACK + dead-letter queue to
// achieve at-least-once, which is required once RabbitMQ can redeliver
// messages across restarts.
//
// ── Concurrency ─────────────────────────────────────────────────────────────
//
// The publish channel (pubCh) is shared across concurrent Publish calls
// and protected by a mutex; amqp.Channel is not goroutine-safe.
// Subscribe channels are per-goroutine — no mutex needed there.
//
// ── Close semantics ──────────────────────────────────────────────────────────
//
// Close() closes the AMQP connection. All blocked Consume loops receive a
// channel-closed signal on msgs and return nil — same graceful exit as
// ChannelBus. ConsumerManager calls Close() only after all consumers have
// exited (ctx cancel → consumer exits → Close()).
package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

const rabbitMQExchange = "cooking.events"

// RabbitMQBus implements EventBus using AMQP 0-9-1.
type RabbitMQBus struct {
	conn   *amqp.Connection
	pubCh  *amqp.Channel // shared publish channel; protected by mu
	mu     sync.Mutex
	closed atomic.Bool
}

// NewRabbitMQBus dials the AMQP broker, opens a publish channel, and
// declares the topic exchange. Returns error if any step fails.
func NewRabbitMQBus(url string, dialTimeout time.Duration) (*RabbitMQBus, error) {
	conn, err := amqp.DialConfig(url, amqp.Config{
		Dial: amqp.DefaultDial(dialTimeout),
	})
	if err != nil {
		return nil, fmt.Errorf("rabbitmq dial: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rabbitmq open publish channel: %w", err)
	}

	// durable=true: exchange survives broker restart.
	if err := ch.ExchangeDeclare(
		rabbitMQExchange,
		"topic",
		true,  // durable
		false, // autoDelete
		false, // internal
		false, // noWait
		nil,
	); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("rabbitmq exchange declare: %w", err)
	}

	zap.L().Info("event bus initialized", zap.String("provider", "rabbitmq"))
	return &RabbitMQBus{conn: conn, pubCh: ch}, nil
}

// Publish serialises evt and publishes to the topic exchange with routing
// key == evt.Topic. Non-blocking on the Go side: amqp091 buffers internally.
func (b *RabbitMQBus) Publish(ctx context.Context, evt Event) error {
	if b.closed.Load() {
		return errors.New("rabbitmq bus: publish on closed bus")
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("rabbitmq bus: marshal event: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pubCh.PublishWithContext(ctx,
		rabbitMQExchange,
		evt.Topic,
		false, // mandatory
		false, // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Transient, // at-most-once for MVP; Step 13 hardens to Persistent
			Body:         body,
		},
	)
}

// Subscribe declares an ephemeral queue bound to topic, then blocks
// consuming until ctx is cancelled or the connection closes.
//
// On ctx cancel, buffered deliveries already in the Go channel are drained
// using context.Background() so handlers complete normally before returning.
func (b *RabbitMQBus) Subscribe(ctx context.Context, topic string, handler Handler) error {
	if b.closed.Load() {
		return errors.New("rabbitmq bus: subscribe on closed bus")
	}

	// Each subscriber gets its own AMQP channel for thread-safety.
	subCh, err := b.conn.Channel()
	if err != nil {
		return fmt.Errorf("rabbitmq subscribe: open channel: %w", err)
	}
	defer subCh.Close()

	// Ephemeral, exclusive queue — server generates a unique name,
	// auto-deleted when consumer disconnects.
	q, err := subCh.QueueDeclare(
		"",    // server-generated name
		false, // durable
		true,  // autoDelete
		true,  // exclusive
		false,
		nil,
	)
	if err != nil {
		return fmt.Errorf("rabbitmq subscribe: queue declare: %w", err)
	}
	if err := subCh.QueueBind(q.Name, topic, rabbitMQExchange, false, nil); err != nil {
		return fmt.Errorf("rabbitmq subscribe: queue bind: %w", err)
	}

	msgs, err := subCh.Consume(
		q.Name,
		"",   // consumer tag (auto-generated)
		true, // autoAck — at-most-once, matches ChannelBus MVP semantics
		true, // exclusive
		false,
		false,
		nil,
	)
	if err != nil {
		return fmt.Errorf("rabbitmq subscribe: consume: %w", err)
	}

	for {
		select {
		case delivery, ok := <-msgs:
			if !ok {
				// Channel closed: connection dropped or Close() called.
				return nil
			}
			var evt Event
			if uerr := json.Unmarshal(delivery.Body, &evt); uerr != nil {
				zap.L().Warn("rabbitmq subscribe: unmarshal",
					zap.String("topic", topic),
					zap.Error(uerr),
				)
				continue
			}
			if herr := handler(ctx, evt.Payload); herr != nil {
				zap.L().Warn("rabbitmq subscribe: handler error",
					zap.String("topic", topic),
					zap.Error(herr),
				)
			}

		case <-ctx.Done():
			// Drain deliveries already buffered in the Go channel before exiting.
			// Uses context.Background() so handlers can complete normally.
			for {
				select {
				case delivery, ok := <-msgs:
					if !ok {
						return nil
					}
					var evt Event
					if uerr := json.Unmarshal(delivery.Body, &evt); uerr != nil {
						zap.L().Warn("rabbitmq subscribe: drain unmarshal",
							zap.String("topic", topic),
							zap.Error(uerr),
						)
						continue
					}
					_ = handler(context.Background(), evt.Payload)
				default:
					return nil
				}
			}
		}
	}
}

// Close closes the AMQP connection. All blocked Subscribe calls receive a
// channel-closed signal and return nil. Idempotent.
func (b *RabbitMQBus) Close() error {
	if b.closed.Swap(true) {
		return nil // already closed
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
