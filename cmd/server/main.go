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
//  12. Graceful shutdown (LIFO, 3 phases — Step 18):
//     Phase 1 (≤5s)   HTTP Server
//     Phase 2 (≤15s)  ConsumerManager → EventBus
//     Phase 3 (≤5s)   OSS Client → Redis → MySQL
//     Overall budget cfg.Server.ShutdownTimeout (default 30s) is the hard cap
//     enforced by the parent context; per-phase deadlines bound the worst-case
//     wait so a hung downstream cannot starve the next phase.
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
	"cooking-platform/pkg/metrics"
	"cooking-platform/pkg/oss"
	"cooking-platform/pkg/sms"
	customvalidator "cooking-platform/pkg/validator"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/plugin/dbresolver"
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

	// ── 2.5 [Step 16] Prometheus metrics initialisation ───────────────────────
	// Must happen before any metric is observed (consumers, middleware, etc.).
	// When cfg.Metrics.Enabled=false we skip Init so /metrics is never registered.
	if cfg.Metrics.Enabled {
		metrics.Init(cfg.Metrics.Namespace)
		log.Info("prometheus metrics initialised", zap.String("namespace", cfg.Metrics.Namespace))
	}

	// ── 3. MySQL ──────────────────────────────────────────────────────────────
	db, err := initMySQL(cfg.Database)
	if err != nil {
		log.Fatal("init mysql", zap.Error(err))
	}
	log.Info("mysql connected")

	// ── 3.5 [Step 16] MySQL pool collector ───────────────────────────────────
	// Registers a pull-based Prometheus collector that reads sql.DB.Stats()
	// on every scrape — no goroutine or periodic timer needed.
	if cfg.Metrics.Enabled {
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			prometheus.MustRegister(metrics.NewMySQLPoolCollector(cfg.Metrics.Namespace, sqlDB))
		}
	}

	// ── 4. Redis ──────────────────────────────────────────────────────────────
	rdb, err := initRedis(cfg.Redis)
	if err != nil {
		log.Fatal("init redis", zap.Error(err))
	}
	log.Info("redis connected")

	// ── 4.5 [Step 16] Redis metrics hook ─────────────────────────────────────
	// AddHook wraps every command call; latency is recorded in RedisCommandDuration.
	if cfg.Metrics.Enabled {
		rdb.AddHook(metrics.NewRedisHook())
	}

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
	userCache := cache.NewUserCache(rdb, cfg.Cache.UserSMSDailyTTL)
	userRepo := repository.NewUserRepository(db)
	// followRepo is constructed early so UserService can resolve is_following
	// on GET /users/:id without depending on the full FollowService.
	followRepo := repository.NewFollowRepository(db)
	jwtMgr := jwtpkg.NewManager(cfg.JWT)

	// [Fix #1] Shared write marker — Post/Follow/User services all stamp it on
	// writes and probe it on reads to force-route to the master for ~5s after a
	// write. Closes the read-after-write race created by DBResolver slave reads.
	writeMarker := cache.NewWriteMarker(rdb, 5*time.Second)

	smsSender, err := sms.NewSender(cfg.SMS)
	if err != nil {
		log.Fatal("init sms sender", zap.Error(err))
	}
	log.Info("sms sender initialized", zap.String("provider", cfg.SMS.Provider))

	userSvc := service.NewUserService(userRepo, followRepo, userCache, writeMarker, jwtMgr, smsSender, cfg.SMS, cfg.OSS, cfg.Encryption, cfg.Ratelimit)
	userHandler := handler.NewUserHandler(userSvc)

	// ── 7.5 [Step 4] Post module wiring ───────────────────────────────────────
	// Step 9 change: NewPostService now also takes cfg.OSS so Create can
	// enforce the OSS whitelist on cover_url + each step's image_urls.
	postRepo := repository.NewPostRepository(db)
	feedCache := cache.NewFeedCache(rdb, cfg.Cache.FeedCacheTTL, cfg.Cache.PVDedupTTL)
	// likeCache lives next to feedCache so PostService / SearchService can
	// enrich list responses with the viewer's liked_by_me state.
	likeCache := cache.NewLikeCache(rdb, cfg.Cache.LikeStateTTL)
	// [Step 7] AuthorAssembler is shared by PostService (feed / author-page)
	// and SearchService — single source of truth for author-snapshot
	// assembly.
	authorAssembler := service.NewAuthorAssembler(userRepo)
	postSvc := service.NewPostService(postRepo, userRepo, feedCache, likeCache, writeMarker, bus, authorAssembler, cfg.OSS, cfg.Cache)
	postHandler := handler.NewPostHandler(postSvc)
	feedHandler := handler.NewFeedHandler(postSvc)
	log.Info("post module wired")

	// ── 7.6 [Step 5] Like module + Consumer wiring ────────────────────────────
	likeRepo := repository.NewLikeRepository(db)
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

	// [Step 18 IDEMP-01] Shared event-id dedup for non-idempotent consumers
	// (PV / Count). LikeConsumer's idempotency is already guarded by Lua at
	// the cache layer; AuditConsumer writes are upserts. Single instance so
	// every consumer hits the same Redis namespace.
	eventDedup := cache.NewEventDedupCache(rdb, cfg.Cache.DedupTTL)

	consumerMgr.Register(consumer.NewLikeConsumer(bus, likeRepo, cfg.Consumer.Like))
	consumerMgr.Register(consumer.NewPVConsumer(bus, db, eventDedup, cfg.Consumer.PV))
	consumerMgr.Register(consumer.NewCountConsumer(bus, db, eventDedup, cfg.Consumer.Count))
	consumerMgr.Register(consumer.NewAuditConsumer(bus, postRepo, auditRepo, auditor, feedCache))
	consumerMgr.StartAll()

	// [Fix #3] DLX depth monitor — RabbitMQ bus only. The ChannelBus
	// implementation does not have a dead-letter queue (in-memory channels
	// drop or block) so the type assertion is the gate. dlxMonitorCtx is the
	// long-lived background context; the goroutine exits at shutdown via
	// cancelDLX below.
	dlxMonitorCtx, cancelDLX := context.WithCancel(context.Background())
	if inspector, ok := bus.(consumer.DLXInspector); ok {
		consumer.StartDLXMonitor(dlxMonitorCtx, inspector)
	}

	// ── 7.7 [Step 7] Search module wiring ─────────────────────────────────────
	searchRepo := repository.NewSearchRepository(db)
	searchSvc := service.NewSearchService(searchRepo, authorAssembler, likeCache, cfg.Search)
	searchHandler := handler.NewSearchHandler(searchSvc)
	log.Info("search module wired")

	// ── 7.8 [Step 8] Follow module wiring ─────────────────────────────────────
	// followRepo is constructed up in stage 7 so UserService can use it; reuse here.
	followSvc := service.NewFollowService(followRepo, userRepo, bus, writeMarker, cfg.Follow)
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
	engine := setupRouter(rdb, userSvc, healthHandler, userHandler, postHandler, feedHandler, likeHandler, searchHandler, followHandler, uploadHandler, cfg.Metrics.Enabled, cfg.CORS)

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

	// ── 12. Graceful shutdown (LIFO, 3 phases) ────────────────────────────────
	// Each phase has its own deadline (5s / 15s / 5s) so a hung downstream
	// cannot starve the next phase. cfg.Server.ShutdownTimeout is the parent
	// budget — phase deadlines never exceed it; remaining slack absorbs
	// goroutine wake-up jitter.
	parentCtx, parentCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer parentCancel()

	// Phase 1: HTTP (≤5s) — refuse new requests, drain in-flight handlers.
	phase1Ctx, phase1Cancel := context.WithTimeout(parentCtx, 5*time.Second)
	if err := srv.Shutdown(phase1Ctx); err != nil {
		log.Warn("phase1 http server shutdown",
			zap.Error(err),
			zap.Duration("phase_budget", 5*time.Second),
		)
	} else {
		log.Info("phase1 http server stopped")
	}
	phase1Cancel()

	// Phase 2: Consumer + EventBus (≤15s) — drain in-flight events, then
	// close the bus so subscribers wake from blocking deliveries.
	phase2Ctx, phase2Cancel := context.WithTimeout(parentCtx, 15*time.Second)
	// [Fix #3] Stop the DLX monitor before the bus is closed; otherwise the
	// goroutine's next QueueDeclare hits a closed channel and logs a noisy WARN.
	cancelDLX()
	if err := consumerMgr.Shutdown(phase2Ctx); err != nil {
		log.Warn("phase2 consumer manager shutdown",
			zap.Error(err),
			zap.Duration("phase_budget", 15*time.Second),
		)
	}
	if err := closeWithDeadline(phase2Ctx, "event bus", bus.Close); err != nil {
		log.Warn("phase2 event bus close", zap.Error(err))
	} else {
		log.Info("phase2 event bus closed")
	}
	phase2Cancel()

	// Phase 3: OSS + Redis + MySQL (≤5s) — infra teardown.
	phase3Ctx, phase3Cancel := context.WithTimeout(parentCtx, 5*time.Second)
	if err := closeWithDeadline(phase3Ctx, "oss client", ossClient.Close); err != nil {
		log.Warn("phase3 oss client close", zap.Error(err))
	} else {
		log.Info("phase3 oss client closed")
	}
	if err := closeWithDeadline(phase3Ctx, "redis", rdb.Close); err != nil {
		log.Warn("phase3 redis close", zap.Error(err))
	} else {
		log.Info("phase3 redis closed")
	}
	if sqlDB, err := db.DB(); err == nil {
		if err := closeWithDeadline(phase3Ctx, "mysql", sqlDB.Close); err != nil {
			log.Warn("phase3 mysql close", zap.Error(err))
		} else {
			log.Info("phase3 mysql closed")
		}
	}
	phase3Cancel()

	log.Info("server exited cleanly")
}

// closeWithDeadline 调用 close()（不带 ctx 的 Close 函数）并以 ctx 的截止时间
// 作为软超时。close 在 goroutine 中执行；ctx 超时则立即返回 ctx.Err()，
// 但 close 仍在后台跑（避免在已损坏的连接上 hang 住整个进程）。
func closeWithDeadline(ctx context.Context, name string, close func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- close()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%s close deadline exceeded: %w", name, ctx.Err())
	}
}

// setupRouter wires HTTP routes for all modules.
//
// Parameters:
//   - rdb: Redis client used only for rate-limit middleware (RateLimitConfig.RDB).
//   - tokenVerifier: satisfied by *service.UserService; decouples middleware from service.
//   - healthHandler: pre-constructed in main() so setupRouter does not need *gorm.DB.
//   - metricsEnabled: when true, register the /metrics endpoint and Metrics() middleware.
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
	metricsEnabled bool,
	corsCfg config.CORSConfig,
) *gin.Engine {
	r := gin.New()

	// Global middlewares (order matters):
	//   Recovery → must be first so it catches panics from everything below.
	//   RequestID → must precede Logger so log lines carry the ID.
	//   Metrics → after Recovery so panics are counted with status 5xx, not 0.
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestID())
	r.Use(middleware.Security())
	r.Use(middleware.Logger())
	r.Use(middleware.CORS(corsCfg))
	if metricsEnabled {
		r.Use(middleware.Metrics())
	}

	// Infrastructure routes (no auth required).
	r.GET("/health", healthHandler.Health)
	r.GET("/readiness", healthHandler.Readiness)
	r.GET("/health/ready", healthHandler.Readiness) // Nginx upstream health check alias
	if metricsEnabled {
		// /metrics exposes Prometheus text format scraped by prometheus service.
		// Not guarded by auth: Prometheus runs on the internal Docker network;
		// Nginx does not proxy /metrics to the public internet.
		r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	}

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
	// OptionalAuth on the public GETs surfaces viewer-specific fields
	// (users/:id → is_following; users/:id/posts → liked_by_me) when a
	// valid Bearer token is attached; anonymous callers still get a 200
	// with those fields defaulted to false.
	userGroup := v1.Group("/users")
	{
		userGroup.GET("/:id", middleware.OptionalAuth(tokenVerifier), userHandler.GetPublicProfile)
		userGroup.GET("/:id/posts", middleware.OptionalAuth(tokenVerifier), feedHandler.ListByUser)

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
		postGroup.DELETE("/:id",
			middleware.Auth(tokenVerifier),
			postHandler.Delete,
		)

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

	// [Step 4] Feed routes — public with optional auth (liked_by_me).
	feedGroup := v1.Group("/feed")
	{
		feedGroup.GET("", middleware.OptionalAuth(tokenVerifier), feedHandler.ListFeed)
	}

	// [Step 7] Search route — public with optional auth (liked_by_me).
	searchGroup := v1.Group("/search")
	{
		searchGroup.GET("",
			middleware.OptionalAuth(tokenVerifier),
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

// initMySQL opens a GORM connection to the master DSN and, when cfg.SlavesDSN
// is non-empty, registers the DBResolver plugin to route SELECT statements to
// replicas (RandomPolicy). Writes (INSERT/UPDATE/DELETE) always go to master.
//
// Pool settings are applied to both master (via sql.DB) and replicas (via
// DBResolver's fluent interface) so connection pressure is distributed evenly.
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

	// [Step 14] Register DBResolver when replicas are configured.
	// RandomPolicy distributes read load evenly across all slaves.
	// When SlavesDSN is empty the plugin is not registered and all
	// traffic stays on master — zero behaviour change for existing code.
	if len(cfg.SlavesDSN) > 0 {
		replicas := make([]gorm.Dialector, len(cfg.SlavesDSN))
		for i, dsn := range cfg.SlavesDSN {
			replicas[i] = mysql.Open(dsn)
		}
		resolver := dbresolver.Register(dbresolver.Config{
			Replicas: replicas,
			Policy:   dbresolver.RandomPolicy{},
		}).
			SetConnMaxIdleTime(cfg.MaxIdleTime).
			SetConnMaxLifetime(cfg.MaxLifetime).
			SetMaxIdleConns(cfg.MaxIdleConns).
			SetMaxOpenConns(cfg.MaxOpenConns)
		if err := db.Use(resolver); err != nil {
			return nil, fmt.Errorf("register dbresolver: %w", err)
		}
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

// initRedis builds the Redis client based on cfg.Mode.
//
// plain (default): single-instance NewClient pointing at cfg.Addr — used by
// dev / docker-compose stacks where Redis is one container.
//
// sentinel ([Fix #5]): NewFailoverClient that consults cfg.SentinelAddrs to
// discover the current master of cfg.MasterName. Used by prod for failover —
// master OOM / restart triggers automatic slave promotion within ~5s and the
// client transparently retries on the new master. The same Password is reused
// for both Redis (data plane) and Sentinel (control plane) since both
// processes are configured with the same auth in deploy/redis/*.conf.tpl.
func initRedis(cfg config.RedisConfig) (*goredis.Client, error) {
	var rdb *goredis.Client
	switch cfg.Mode {
	case "sentinel":
		rdb = goredis.NewFailoverClient(&goredis.FailoverOptions{
			MasterName:       cfg.MasterName,
			SentinelAddrs:    cfg.SentinelAddrs,
			Password:         cfg.Password,
			SentinelPassword: cfg.Password,
			DB:               cfg.DB,
			PoolSize:         cfg.PoolSize,
			DialTimeout:      cfg.DialTimeout,
			ReadTimeout:      cfg.ReadTimeout,
			WriteTimeout:     cfg.WriteTimeout,
		})
	default: // "plain" or ""
		rdb = goredis.NewClient(&goredis.Options{
			Addr:         cfg.Addr,
			Password:     cfg.Password,
			DB:           cfg.DB,
			PoolSize:     cfg.PoolSize,
			DialTimeout:  cfg.DialTimeout,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
		})
	}

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
		return event.NewRabbitMQBus(cfg)
	default:
		return nil, fmt.Errorf("unknown mq provider: %q (supported: channel, rabbitmq)", cfg.Provider)
	}
}
