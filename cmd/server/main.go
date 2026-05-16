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
//     7.7  Search module wiring                  (Step 7)
//     7.8  Follow module wiring                  (Step 8)
//     7.9  Upload module wiring                  (Step 9)
//  8. Custom validator registration         (Step 3)
//  9. gin.SetMode + setupRouter
//  10. HTTP server start
//  11. Wait for SIGINT/SIGTERM
//  12. Graceful shutdown (LIFO):
//     HTTP Server → ConsumerManager → OSS Client → EventBus → Redis → MySQL
//
// Step 9 added stage 7.9 because the upload module needs an oss.Client that
// MockClient backs with an embedded HTTP listener — its lifecycle (start in
// NewClient, stop in Close) must be managed alongside the other process-wide
// resources. OSS Client closes BEFORE EventBus so any in-flight upload bytes
// finish landing before we tear down lower-level services.
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
	"cooking-platform/pkg/audit"
	"cooking-platform/pkg/config"
	jwtpkg "cooking-platform/pkg/jwt"
	"cooking-platform/pkg/logger"
	"cooking-platform/pkg/oss"
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
		zap.String("oss_provider", cfg.OSS.Provider),
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
	consumerMgr := consumer.NewManager()

	// ── 7. [Step 3] User module wiring ────────────────────────────────────────
	// Step 9 change: NewUserService now also takes cfg.OSS so UpdateProfile
	// can enforce the OSS whitelist on avatar_url updates.
	userCache := cache.NewUserCache(rdb)
	userRepo := repository.NewUserRepository(db)
	jwtMgr := jwtpkg.NewManager(cfg.JWT)

	smsSender, err := sms.NewSender(cfg.SMS)
	if err != nil {
		log.Fatal("init sms sender", zap.Error(err))
	}
	log.Info("sms sender initialized", zap.String("provider", cfg.SMS.Provider))

	userSvc := service.NewUserService(userRepo, userCache, jwtMgr, smsSender, cfg.SMS, cfg.OSS, cfg.Encryption, cfg.Ratelimit)
	userHandler := handler.NewUserHandler(userSvc)

	// ── 7.5 [Step 4] Post module wiring ───────────────────────────────────────
	// Step 9 change: NewPostService now also takes cfg.OSS so Create can
	// enforce the OSS whitelist on cover_url + each step's image_urls.
	postRepo := repository.NewPostRepository(db)
	feedCache := cache.NewFeedCache(rdb, cfg.Cache.FeedCacheTTL, cfg.Cache.PVDedupTTL)
	// [Step 7] AuthorAssembler is shared by PostService (feed / author-page)
	// and SearchService — single source of truth for author-snapshot
	// assembly.
	authorAssembler := service.NewAuthorAssembler(userRepo)
	postSvc := service.NewPostService(postRepo, userRepo, feedCache, bus, authorAssembler, cfg.OSS, cfg.Cache)
	postHandler := handler.NewPostHandler(postSvc)
	feedHandler := handler.NewFeedHandler(postSvc)
	log.Info("post module wired")

	// ── 7.6 [Step 5] Like module + Consumer wiring ────────────────────────────
	likeRepo := repository.NewLikeRepository(db)
	likeCache := cache.NewLikeCache(rdb, cfg.Cache.LikeStateTTL)
	likeSvc := service.NewLikeService(postRepo, likeCache, bus)
	likeHandler := handler.NewLikeHandler(likeSvc)
	log.Info("like module wired")

	// ── 7.10 [Step 10] Audit module wiring ───────────────────────────────────
	// auditRepo and auditor are initialised here (before StartAll) so
	// AuditConsumer can be registered in the same StartAll batch as the
	// other consumers. feedCache and postRepo are already available from 7.5.
	auditRepo := repository.NewAuditRepository(db)
	auditor, err := audit.NewAuditor(cfg.Audit)
	if err != nil {
		log.Fatal("init auditor", zap.Error(err))
	}
	log.Info("auditor initialized", zap.String("provider", cfg.Audit.Provider))

	consumerMgr.Register(consumer.NewLikeConsumer(bus, likeRepo, cfg.Consumer.Like))
	consumerMgr.Register(consumer.NewPVConsumer(bus, db, cfg.Consumer.PV))
	consumerMgr.Register(consumer.NewCountConsumer(bus, db, cfg.Consumer.Count))
	consumerMgr.Register(consumer.NewAuditConsumer(bus, postRepo, auditRepo, auditor, feedCache))
	consumerMgr.StartAll()

	// ── 7.7 [Step 7] Search module wiring ─────────────────────────────────────
	searchRepo := repository.NewSearchRepository(db)
	searchSvc := service.NewSearchService(searchRepo, authorAssembler)
	searchHandler := handler.NewSearchHandler(searchSvc)
	log.Info("search module wired")

	// ── 7.8 [Step 8] Follow module wiring ─────────────────────────────────────
	followRepo := repository.NewFollowRepository(db)
	followSvc := service.NewFollowService(followRepo, userRepo, bus)
	followHandler := handler.NewFollowHandler(followSvc)
	log.Info("follow module wired")

	// ── 7.9 [Step 9] Upload module wiring ─────────────────────────────────────
	// Order:
	//   a. oss.NewClient → MockClient (starts embedded HTTP listener on
	//      cfg.OSS.MockListenAddr) or AliyunClient (no goroutines).
	//   b. UploadCache wraps Redis for nonce storage.
	//   c. UploadService composes (a) + (b) + cfg.OSS.
	//   d. UploadHandler wraps the service for HTTP routing.
	//
	// We deliberately wire upload AFTER the other modules so a misconfigured
	// OSS section can't break the rest of the boot path — the server still
	// comes up if upload fails to initialise. But we treat that as fatal
	// for now: a server without upload is functionally broken for the
	// Step 9 deliverable. Loosen this when graceful degradation matters.
	ossClient, err := oss.NewClient(cfg.OSS)
	if err != nil {
		log.Fatal("init oss client", zap.Error(err))
	}
	uploadCache := cache.NewUploadCache(rdb)
	uploadSvc := service.NewUploadService(ossClient, uploadCache, cfg.OSS)
	uploadHandler := handler.NewUploadHandler(uploadSvc)
	log.Info("upload module wired", zap.String("provider", cfg.OSS.Provider))

	// ── 8. Custom validator registration ──────────────────────────────────────
	if err := customvalidator.Register(); err != nil {
		log.Fatal("register custom validators", zap.Error(err))
	}
	log.Info("custom validators registered")

	// ── 9. Gin engine ─────────────────────────────────────────────────────────
	// HealthHandler is created here so setupRouter does not need *gorm.DB.
	healthHandler := handler.NewHealthHandler(db, rdb)
	gin.SetMode(cfg.Server.Mode)
	engine := setupRouter(rdb, userSvc, healthHandler, userHandler, postHandler, feedHandler, likeHandler, searchHandler, followHandler, uploadHandler)

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
	// HTTP Server → ConsumerManager → OSS Client → EventBus → Redis → MySQL.
	//
	// HTTP first so no new events are published while consumers drain.
	// Consumers next so all in-flight events finish persisting.
	// OSS Client then so MockClient's embedded HTTP listener stops accepting
	// uploads (no behavioural effect on AliyunClient — Close is a no-op there).
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

	if err := ossClient.Close(); err != nil {
		log.Error("oss client close", zap.Error(err))
	} else {
		log.Info("oss client closed")
	}

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
// Parameters:
//   - rdb: Redis client used only for rate-limit middleware (RateLimitConfig.RDB).
//   - tokenVerifier: satisfied by *service.UserService; decouples middleware from service.
//   - healthHandler: pre-constructed in main() so setupRouter does not need *gorm.DB.
//
// Signature evolution: each new module adds its handler as a parameter.
// Keep all routes in one function so the full API surface is visible at a glance.
func setupRouter(
	rdb *goredis.Client,
	tokenVerifier middleware.TokenVerifier,
	healthHandler *handler.HealthHandler,
	userHandler *handler.UserHandler,
	postHandler *handler.PostHandler,
	feedHandler *handler.FeedHandler,
	likeHandler *handler.LikeHandler,
	searchHandler *handler.SearchHandler,
	followHandler *handler.FollowHandler,
	uploadHandler *handler.UploadHandler,
) *gin.Engine {
	r := gin.New()

	// Global middlewares (order matters):
	//   Recovery → must be first so it catches panics from everything below.
	//   RequestID → must precede Logger so log lines carry the ID.
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.Security())
	r.Use(middleware.Logger())
	r.Use(middleware.CORS())

	// Infrastructure routes (no auth required).
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
		authGroup.POST("/logout", middleware.Auth(tokenVerifier), userHandler.Logout)
	}

	// [Step 3+4+8] User routes — mixed visibility.
	userGroup := v1.Group("/users")
	{
		userGroup.GET("/:id", userHandler.GetPublicProfile)
		userGroup.GET("/:id/posts", feedHandler.ListByUser)

		// [Step 8] Follow resource on a user.
		userGroup.POST("/:id/follow", middleware.Auth(tokenVerifier), followHandler.Follow)
		userGroup.DELETE("/:id/follow", middleware.Auth(tokenVerifier), followHandler.Unfollow)
		userGroup.GET("/:id/followers", followHandler.ListFollowers)
		userGroup.GET("/:id/following", followHandler.ListFollowing)

		// Protected — current user only.
		meGroup := userGroup.Group("/me", middleware.Auth(tokenVerifier))
		{
			meGroup.GET("", userHandler.GetMyProfile)
			meGroup.PATCH("", userHandler.UpdateProfile)
		}
	}

	// [Step 4+5] Post routes.
	postGroup := v1.Group("/posts")
	{
		postGroup.POST("",
			middleware.Auth(tokenVerifier),
			middleware.RateLimit(middleware.RateLimitConfig{
				RDB:     rdb,
				KeyFunc: middleware.PerUserKey("limit:pub"),
				Limit:   20,
				Window:  24 * time.Hour,
			}),
			postHandler.Create,
		)
		postGroup.GET("/:id", postHandler.GetDetail)

		postGroup.POST("/:id/like",
			middleware.Auth(tokenVerifier),
			middleware.RateLimit(middleware.RateLimitConfig{
				RDB:     rdb,
				KeyFunc: middleware.PerUserKey("limit:like"),
				Limit:   200,
				Window:  24 * time.Hour,
			}),
			likeHandler.Like,
		)
		postGroup.DELETE("/:id/like",
			middleware.Auth(tokenVerifier),
			likeHandler.Unlike,
		)
		postGroup.GET("/:id/like",
			middleware.Auth(tokenVerifier),
			likeHandler.GetLikeStatus,
		)
	}

	// [Step 4] Feed routes — public.
	feedGroup := v1.Group("/feed")
	{
		feedGroup.GET("", feedHandler.ListFeed)
	}

	// [Step 7] Search route — public.
	searchGroup := v1.Group("/search")
	{
		searchGroup.GET("",
			middleware.RateLimit(middleware.RateLimitConfig{
				RDB:     rdb,
				KeyFunc: middleware.PerIPKey("limit:search"),
				Limit:   30,
				Window:  60 * time.Second,
			}),
			searchHandler.Search,
		)
	}

	// [Step 9] Upload routes — auth required, rate-limited per user.
	//
	// /presign is rate-limited (30 req/hour per user) — a hostile client
	// could otherwise flood OSS with presigned URLs. /callback is NOT
	// rate-limited: it's already gated by single-use nonce + ownership
	// check, and a callback corresponds to an upload the user already
	// PUT, so abuse would just delete their own nonces.
	uploadGroup := v1.Group("/upload", middleware.Auth(tokenVerifier))
	{
		uploadGroup.POST("/presign",
			middleware.RateLimit(middleware.RateLimitConfig{
				RDB:     rdb,
				KeyFunc: middleware.PerUserKey("limit:upload"),
				Limit:   30,
				Window:  time.Hour,
			}),
			uploadHandler.Presign,
		)
		uploadGroup.POST("/callback", uploadHandler.Callback)
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

// initEventBus selects the EventBus implementation from config.
func initEventBus(cfg config.MQConfig) (event.EventBus, error) {
	switch cfg.Provider {
	case "channel", "":
		return event.NewChannelBus(1024), nil
	case "rabbitmq":
		if cfg.URL == "" {
			return nil, fmt.Errorf("mq.url is required when provider=rabbitmq")
		}
		return event.NewRabbitMQBus(cfg.URL, cfg.Timeout)
	default:
		return nil, fmt.Errorf("unknown mq provider: %q (supported: channel, rabbitmq)", cfg.Provider)
	}
}
