// Package consumer — dlx_monitor.go polls the RabbitMQ dead-letter queue on
// a fixed cadence and surfaces its depth as a Prometheus gauge.
// ([Fix #3] message-graveyard monitoring.)
//
// Why this is needed:
//   The bus is configured so that any Nack(requeue=false) — which is what
//   every consumer does for unmarshal failures and handler errors — routes
//   the message to "cooking.events.dlx.queue". Without monitoring, that
//   queue grows silently: messages sit forever, RabbitMQ disk usage creeps
//   up, and the operator has no idea anything is wrong until either the
//   broker runs out of disk or someone hand-inspects the queue.
//
//   The monitor closes that loop: it pushes the depth into Prometheus on a
//   30s cadence and Alertmanager pages on the DLXNotEmpty rule (see
//   deploy/prometheus/alerts.yml).
//
// Why not piggyback on a Consumer:
//   The pattern in this package is "Consumer = topic subscriber". The DLX
//   monitor never reads messages — it only inspects the queue's depth via
//   QueueDeclare. Mixing the two into one type would muddy that contract,
//   so it lives as a standalone goroutine started from main.go and stopped
//   by its own context.
package consumer

import (
	"context"
	"time"

	"cooking-platform/pkg/metrics"

	"go.uber.org/zap"
)

// dlxMonitorInterval is the polling cadence. 30s is the sweet spot: short
// enough that an operator notices the queue growing within one Grafana
// refresh, long enough that a misbehaving broker isn't asked for inspection
// dozens of times per minute.
const dlxMonitorInterval = 30 * time.Second

// DLXInspector is the narrow capability the monitor needs from the bus —
// RabbitMQBus implements it; ChannelBus does not (and the monitor is never
// started in that case). Defined here rather than in package event to
// avoid cycling event → consumer for a one-off observer.
type DLXInspector interface {
	DLXQueueDepth() (int, error)
}

// StartDLXMonitor launches a goroutine that polls inspector every
// dlxMonitorInterval and updates metrics.DLXQueueDepth. The goroutine exits
// when ctx is cancelled.
//
// Logs an ERROR-level line whenever the depth is observed > 0 — this is the
// belt to the suspenders of the Alertmanager rule: even if metrics scraping
// is down for a moment, the message-graveyard event still hits the log
// pipeline. WARN-level on inspect errors so an unreachable broker doesn't
// drown the log at ERROR.
func StartDLXMonitor(ctx context.Context, inspector DLXInspector) {
	go func() {
		ticker := time.NewTicker(dlxMonitorInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				zap.L().Info("dlx monitor stopped")
				return
			case <-ticker.C:
				depth, err := inspector.DLXQueueDepth()
				if err != nil {
					zap.L().Warn("dlx monitor: inspect failed", zap.Error(err))
					continue
				}
				if metrics.DLXQueueDepth != nil {
					metrics.DLXQueueDepth.Set(float64(depth))
				}
				if depth > 0 {
					zap.L().Error("dlx queue not empty — messages awaiting inspection",
						zap.Int("depth", depth),
					)
				}
			}
		}
	}()
	zap.L().Info("dlx monitor started", zap.Duration("interval", dlxMonitorInterval))
}
