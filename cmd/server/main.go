// Command server is the application entrypoint.
// Boot sequence: config → logger → MySQL → Redis → EventBus → ConsumerManager → Gin → HTTP server → graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cooking-platform/internal/consumer"
	"cooking-platform/internal/event"
	"cooking-platform/internal/handler"
	"cooking-platform/internal/middleware"
	"cooking-platform/pkg/config"
	"cooking-platform/pkg/logger"

	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func main() {
	// ── 1. Configuration ──────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: load config: %v\n", err)
		os.Exit(1)
	}

	// ── 2. Logger ─────────────────────────────────────────────────────────────
	log, err := logger.Init(cfg.Log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: init logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck

	log.Info("cooking-platform starting",
		zap.String("mode", cfg.Server.Mode),
		zap.Int("port", cfg.Server.Port),
		zap.String("mq_provider", cfg.MQ.Provider),
	)

	// ── 3. MySQL ──────────────────────────────────────────────────────────────
	db, err := initMySQL(cfg.Database)
	if err != nil {
		log.Fatal("init mysql", zap.Error(err))
	}
	log.Info("mysql connected")

	// ── 4. Redis ──────────────────────────────────────────────────────────────
	rdb, err := initRedis(cfg.Redis)
	if err != nil {
		log.Fatal("init redis", zap.Error(err))
	}
	log.Info("redis connected")

	// ── 5. EventBus ───────────────────────────────────────────────────────────
	// [第 2 步新增] 根据 cfg.MQ.Provider 选择实现：
	//   "channel"  → 进程内 Go Channel（MVP，零外部依赖）
	//   "rabbitmq" → 第 13 步实现，当前配置此值会快速失败
	bus, err := initEventBus(cfg.MQ)
	if err != nil {
		log.Fatal("init event bus", zap.Error(err))
	}
	log.Info("event bus initialized", zap.String("provider", cfg.MQ.Provider))

	// ── 6. ConsumerManager ────────────────────────────────────────────────────
	// [第 2 步新增] 当前无业务 Consumer，StartAll 启动 0 个 goroutine。
	// 第 5 步起在此处 Register 业务 Consumer：
	//   consumerMgr.Register(consumer.NewLikeConsumer(bus, db, rdb))
	consumerMgr := consumer.NewManager()
	consumerMgr.StartAll()

	// ── 7. Gin engine ─────────────────────────────────────────────────────────
	gin.SetMode(cfg.Server.Mode)
	engine := setupRouter(db, rdb)

	// ── 8. HTTP server ────────────────────────────────────────────────────────
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      engine,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	go func() {
		log.Info("http server listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("http server error", zap.Error(err))
		}
	}()

	// ── 9. Graceful shutdown ──────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Info("shutdown signal received", zap.String("signal", sig.String()))

	// [第 2 步修改] 关闭顺序：HTTP Server → ConsumerManager → EventBus → Redis → MySQL
	// 原则：先停流量入口，再停异步消费，再关基础设施，保证无消息丢失。

	// 9-a. HTTP Server：给存量请求 30 秒排空
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http server shutdown error", zap.Error(err))
	}
	log.Info("http server stopped")

	// 9-b. ConsumerManager：cancel ctx → 等所有 Consumer 处理完当前消息退出
	consumerMgr.Shutdown()

	// 9-c. EventBus：Consumer 全部退出后再关闭，无竞态
	if err := bus.Close(); err != nil {
		log.Error("event bus close error", zap.Error(err))
	}

	// 9-d. Redis
	if err := rdb.Close(); err != nil {
		log.Error("redis close error", zap.Error(err))
	}
	log.Info("redis close ok")

	// 9-e. MySQL
	sqlDB, _ := db.DB()
	if err := sqlDB.Close(); err != nil {
		log.Error("mysql close error", zap.Error(err))
	}
	log.Info("mysql close ok")

	log.Info("server exited cleanly")
}

// setupRouter builds the gin.Engine, registers global middleware, and mounts routes.
func setupRouter(db *gorm.DB, rdb *goredis.Client) *gin.Engine {
	r := gin.New()

	r.Use(middleware.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.Logger())
	r.Use(middleware.CORS())

	healthHandler := handler.NewHealthHandler(db, rdb)
	r.GET("/health", healthHandler.Health)
	r.GET("/readiness", healthHandler.Readiness)

	// v1 := r.Group("/api/v1")

	return r
}

// initMySQL 与第 1 步完全一致，未做任何修改。
func initMySQL(cfg config.DatabaseConfig) (*gorm.DB, error) {
	gormLevel := gormlogger.Warn
	switch cfg.LogLevel {
	case "silent":
		gormLevel = gormlogger.Silent
	case "error":
		gormLevel = gormlogger.Error
	case "info":
		gormLevel = gormlogger.Info
	}

	db, err := gorm.Open(mysql.Open(cfg.DSN), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("gorm open: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}

	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.MaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.MaxIdleTime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("mysql ping: %w", err)
	}

	return db, nil
}

// initRedis 与第 1 步完全一致，未做任何修改。
func initRedis(cfg config.RedisConfig) (*goredis.Client, error) {
	rdb := goredis.NewClient(&goredis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := rdb.Ping(ctx).Result(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return rdb, nil
}

// initEventBus 根据配置选择 EventBus 实现。
// [第 2 步新增]
//
//	"channel"  → ChannelBus（MVP，进程内，零外部依赖）
//	"rabbitmq" → 第 13 步实现，当前返回 error 快速失败防止误配置上线
func initEventBus(cfg config.MQConfig) (event.EventBus, error) {
	switch cfg.Provider {
	case "channel", "":
		return event.NewChannelBus(1024), nil
	case "rabbitmq":
		return nil, errors.New("rabbitmq event bus not implemented yet, set mq.provider=channel for MVP")
	default:
		return nil, fmt.Errorf("unknown mq provider: %q (supported: channel, rabbitmq)", cfg.Provider)
	}
}
