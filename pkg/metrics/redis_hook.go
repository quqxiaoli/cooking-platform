package metrics

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisHook implements redis.Hook and records per-command latency into
// RedisCommandDuration. Register it on the go-redis client with AddHook().
//
// Pipeline commands are not individually timed (they share one network round
// trip); only the aggregate pipeline call is visible in the hook. We skip
// pipeline timing to avoid misleading sub-millisecond per-command samples.
type RedisHook struct{}

// NewRedisHook returns a hook ready to be passed to rdb.AddHook().
func NewRedisHook() *RedisHook { return &RedisHook{} }

// DialHook satisfies redis.Hook; we don't instrument dial latency.
func (h *RedisHook) DialHook(next redis.DialHook) redis.DialHook { return next }

// ProcessHook wraps every single-command call with latency recording.
func (h *RedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)

		if RedisCommandDuration == nil {
			return err // metrics not yet initialised (test environments)
		}

		status := "ok"
		// redis.Nil is a normal "key not found" signal, not an error.
		if err != nil && !errors.Is(err, redis.Nil) {
			status = "error"
		}
		RedisCommandDuration.
			WithLabelValues(cmd.Name(), status).
			Observe(time.Since(start).Seconds())
		return err
	}
}

// ProcessPipelineHook satisfies redis.Hook; pipeline commands are not timed.
func (h *RedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		return next(ctx, cmds)
	}
}
