// Package main is the cooking-platform entry point.
//
// Boot sequence (see numbered comments below for the canonical order):
//
//  1. Configuration
//  2. Logger
//  3. MySQL
//  4. Redis
//  5. EventBus
//  6. ConsumerManager — created empty; consumers register at 7.6
//  7. User module wiring                    (Step 3)
//     7.5  Post module wiring                    (Step 4)
//     7.6  Like module + Consumer wiring + StartAll  (Step 5)
//  8. Custom validator registration         (Step 3)
//  9. gin.SetMode + setupRouter
//  10. HTTP server start
//  11. Wait for SIGINT/SIGTERM
//  12. Graceful shutdown (LIFO):
//     HTTP Server → ConsumerManager → EventBus → Redis → MySQL
//
// Step 5 added stage 7.6 because consumers depend on the EventBus (stage 5)
// AND the repositories (stages 7 / 7.5). Putting StartAll at the end of
// 7.6 — after all Register calls — is mandatory; the empty Manager at
// stage 6 is just a placeholder so the variable is in scope for shutdown.
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

	"cooking-platform/internal/cache"
	"cooking-platform/internal/consumer"
	"cooking-platform/internal/event"
	"cooking-platform/internal/handler"
	"cooking-platform/internal/middleware"
	"cooking-platform/internal/repository"
	"cooking-platform/internal/service"
	"cooking-platform/pkg/config"
	jwtpkg "cooking-platform/pkg/jwt"
	"cooking-platform/pkg/logger"
	"cooking-platform/pkg/sms"
	customvalidator "cooking-platform/pkg/validator"

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
		zap.String("sms_provider", cfg.SMS.Provider),
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
	bus, err := initEventBus(cfg.MQ)
	if err != nil {
		log.Fatal("init event bus", zap.Error(err))
	}
	log.Info("event bus initialized", zap.String("provider", cfg.MQ.Provider))

	// ── 6. ConsumerManager (empty placeholder) ────────────────────────────────
	// Step 5 change: consumers are registered in stage 7.6 (after their
	// repository dependencies are constructed), then StartAll is invoked
	// at the END of 7.6 — not here. We still create the manager up-front
	// so its variable is captured in the same scope as the shutdown block.
	consumerMgr := consumer.NewManager()

	// ── 7. [Step 3] User module wiring ────────────────────────────────────────
	// Order: cache → repo → JWT manager → SMS sender → service → handler.
	// Each layer depends only on what was constructed before it.
	userCache := cache.NewUserCache(rdb)
	userRepo := repository.NewUserRepository(db)
	jwtMgr := jwtpkg.NewManager(cfg.JWT)

	smsSender, err := sms.NewSender(cfg.SMS)
	if err != nil {
		log.Fatal("init sms sender", zap.Error(err))
	}
	log.Info("sms sender initialized", zap.String("provider", cfg.SMS.Provider))

	userSvc := service.NewUserService(userRepo, userCache, jwtMgr, smsSender, cfg.SMS, cfg.Ratelimit)
	userHandler := handler.NewUserHandler(userSvc)

	// ── 7.5 [Step 4] Post module wiring ───────────────────────────────────────
	// Order: cache → repo → service → handler. Each layer depends only on
	// what was constructed before it. PostService takes the userRepo from
	// Step 3 to embed author snapshots in feed/detail responses.
	//
	// PostService publishes via the EventPublisher interface (not the full
	// EventBus) so the dependency direction is clean: service emits events,
	// it does not subscribe to them. Subscription belongs to consumers,
	// which are wired separately at stage 7.6.
	postRepo := repository.NewPostRepository(db)
	feedCache := cache.NewFeedCache(rdb)
	postSvc := service.NewPostService(postRepo, userRepo, feedCache, bus)
	postHandler := handler.NewPostHandler(postSvc)
	feedHandler := handler.NewFeedHandler(postSvc)
	log.Info("post module wired")

	// ── 7.6 [Step 5] Like module + Consumer wiring ────────────────────────────
	// Order:
	//   a. Like module dependencies (cache → repo → service → handler).
	//   b. Register the THREE consumers (LikeConsumer, PVConsumer, CountConsumer)
	//      with the manager. They all need the EventSubscriber side of `bus`
	//      and write to the master MySQL via the existing repos / raw SQL.
	//   c. StartAll: now-and-only-now we boot the consumer goroutines.
	//      Doing it here (not earlier) ensures every consumer's deps exist;
	//      doing it BEFORE setupRouter ensures consumers are draining the
	//      bus before the first HTTP request can publish.
	likeRepo := repository.NewLikeRepository(db)
	likeCache := cache.NewLikeCache(rdb)
	likeSvc := service.NewLikeService(postRepo, likeCache, bus)
	likeHandler := handler.NewLikeHandler(likeSvc)
	log.Info("like module wired")

	consumerMgr.Register(consumer.NewLikeConsumer(bus, likeRepo))
	consumerMgr.Register(consumer.NewPVConsumer(bus, db))
	consumerMgr.Register(consumer.NewCountConsumer(bus, db))
	consumerMgr.StartAll()

	// ── 8. Custom validator registration ──────────────────────────────────────
	// Must run BEFORE setupRouter so any DTO using `binding:"phone"` works.
	if err := customvalidator.Register(); err != nil {
		log.Fatal("register custom validators", zap.Error(err))
	}
	log.Info("custom validators registered")

	// ── 9. Gin engine ─────────────────────────────────────────────────────────
	gin.SetMode(cfg.Server.Mode)
	engine := setupRouter(db, rdb, userSvc, userHandler, postHandler, feedHandler, likeHandler)

	// ── 10. HTTP server ───────────────────────────────────────────────────────
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

	// ── 11. Wait for shutdown signal ──────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutdown signal received")

	// ── 12. Graceful shutdown (LIFO order) ────────────────────────────────────
	// HTTP Server → ConsumerManager → EventBus → Redis → MySQL.
	//
	// HTTP first so no new events are published while consumers drain.
	// Consumer manager next so all in-flight events finish persisting.
	// EventBus then closes so any latent Publish call returns error
	// instead of blocking. Finally the infrastructure connections.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http server shutdown", zap.Error(err))
	} else {
		log.Info("http server stopped")
	}

	log.Info("consumer manager shutting down...")
	consumerMgr.Shutdown()
	log.Info("consumer manager stopped")

	if err := bus.Close(); err != nil {
		log.Error("event bus close", zap.Error(err))
	} else {
		log.Info("event bus closed")
	}

	if err := rdb.Close(); err != nil {
		log.Error("redis close", zap.Error(err))
	} else {
		log.Info("redis close ok")
	}

	if sqlDB, err := db.DB(); err == nil {
		if err := sqlDB.Close(); err != nil {
			log.Error("mysql close", zap.Error(err))
		} else {
			log.Info("mysql close ok")
		}
	}

	log.Info("server exited cleanly")
}

// setupRouter wires HTTP routes for all modules.
//
// Signature evolution: each module added in subsequent steps brings its own
// service/handler pair as parameters. Keep all routes in this single function
// so the URL structure of the API is visible at one glance.
//
// [Step 3] adds: userSvc (for Auth middleware), userHandler (for v1 routes).
// [Step 4] adds: postHandler (POST /posts, GET /posts/:id),
//
//	feedHandler (GET /feed, GET /users/:id/posts).
//
// [Step 5] adds: likeHandler (POST/DELETE/GET /posts/:id/like).
func setupRouter(
	db *gorm.DB,
	rdb *goredis.Client,
	userSvc *service.UserService,
	userHandler *handler.UserHandler,
	postHandler *handler.PostHandler,
	feedHandler *handler.FeedHandler,
	likeHandler *handler.LikeHandler,
) *gin.Engine {
	r := gin.New()

	// Global middlewares (order matters):
	//   Recovery → must be first so it catches panics from everything below.
	//   RequestID → must precede Logger so log lines carry the ID.
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.Logger())
	r.Use(middleware.CORS())

	// Infrastructure routes (no auth required).
	healthHandler := handler.NewHealthHandler(db, rdb)
	r.GET("/health", healthHandler.Health)
	r.GET("/readiness", healthHandler.Readiness)

	// ── /api/v1 group ─────────────────────────────────────────────────────────
	v1 := r.Group("/api/v1")

	// [Step 3] Auth routes — public (no JWT required).
	authGroup := v1.Group("/auth")
	{
		authGroup.POST("/send-code", userHandler.SendCode)
		authGroup.POST("/login", userHandler.Login)
		authGroup.POST("/refresh", userHandler.Refresh)
		// Logout requires a valid token (we need its JTI to blacklist).
		authGroup.POST("/logout", middleware.Auth(userSvc), userHandler.Logout)
	}

	// [Step 3+4] User routes — mixed visibility.
	userGroup := v1.Group("/users")
	{
		// Public — anyone can view a user's public profile.
		userGroup.GET("/:id", userHandler.GetPublicProfile)

		// [Step 4] Public — author timeline (cursor-paginated).
		// Note: this lives under /users/:id/ rather than /feed?author=:id
		// to mirror the PRD-Phase3 §7.1 follow-route style
		// (/users/:id/followers, /users/:id/following).
		userGroup.GET("/:id/posts", feedHandler.ListByUser)

		// Protected — current user only.
		meGroup := userGroup.Group("/me", middleware.Auth(userSvc))
		{
			meGroup.GET("", userHandler.GetMyProfile)
			meGroup.PATCH("", userHandler.UpdateProfile)
		}
	}

	// [Step 4+5] Post routes — write requires auth + rate limit; reads are public.
	//
	// Rate-limit policy: limit:pub:{user_id}, 20 posts per 24h, sliding window.
	// PRD-Phase3 §6.3 specifies this cap to deter spam without throttling
	// reasonable creators (20/day = one every 72 minutes on average).
	//
	// The limit is enforced AFTER Auth — chain order matters: an anonymous
	// request gets a 401 (cheaper) before any Redis trip happens.
	//
	// [Step 5] Like routes hang under /posts/:id/like:
	//   POST   → like        (auth + rate-limit: 200 likes / 24h)
	//   DELETE → unlike      (auth only — taking back is always allowed)
	//   GET    → like-status (auth only — answers "have *I* liked this")
	//
	// Why like POST is rate-limited but unlike isn't: the abuse vector
	// is mass-spamming hearts (boosts vanity metrics, triggers fake
	// notifications). Mass-unliking has no comparable abuse model;
	// rate-limiting it would just slow legitimate "I changed my mind"
	// flows. PRD-Phase3 §6.3 sets limit:like at 200/24h, well above
	// any human use pattern.
	postGroup := v1.Group("/posts")
	{
		postGroup.POST("",
			middleware.Auth(userSvc),
			middleware.RateLimit(middleware.RateLimitConfig{
				RDB:     rdb,
				KeyFunc: middleware.PerUserKey("limit:pub"),
				Limit:   20,
				Window:  24 * time.Hour,
			}),
			postHandler.Create,
		)
		// Detail is public so feed cards (which may include unauth users)
		// can deep-link into a post without forcing login.
		postGroup.GET("/:id", postHandler.GetDetail)

		// [Step 5] Like resource on a post.
		postGroup.POST("/:id/like",
			middleware.Auth(userSvc),
			middleware.RateLimit(middleware.RateLimitConfig{
				RDB:     rdb,
				KeyFunc: middleware.PerUserKey("limit:like"),
				Limit:   200,
				Window:  24 * time.Hour,
			}),
			likeHandler.Like,
		)
		postGroup.DELETE("/:id/like",
			middleware.Auth(userSvc),
			likeHandler.Unlike,
		)
		postGroup.GET("/:id/like",
			middleware.Auth(userSvc),
			likeHandler.GetLikeStatus,
		)
	}

	// [Step 4] Feed routes — public.
	feedGroup := v1.Group("/feed")
	{
		feedGroup.GET("", feedHandler.ListFeed)
	}

	return r
}

// initMySQL is unchanged from Step 1.
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

// initRedis is unchanged from Step 1.
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

// initEventBus is unchanged from Step 2.
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
