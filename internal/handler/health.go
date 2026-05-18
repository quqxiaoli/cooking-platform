// Package handler contains all gin HTTP handlers.
// Each handler struct depends only on interfaces/concrete types injected at startup.
package handler

import (
	"context"
	"time"

	"cooking-platform/pkg/response"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// HealthHandler handles infrastructure health and readiness probes.
// These endpoints are called by:
//   - Load balancers (Nginx upstream health check)
//   - Kubernetes/Docker healthcheck directives
//   - On-call engineers debugging a degraded pod
type HealthHandler struct {
	db  *gorm.DB
	rdb *redis.Client
}

// NewHealthHandler constructs a HealthHandler with live DB and Redis clients.
func NewHealthHandler(db *gorm.DB, rdb *redis.Client) *HealthHandler {
	return &HealthHandler{db: db, rdb: rdb}
}

// Health is a lightweight liveness probe. It returns 200 immediately without
// touching any downstream dependency. If the process can serve this endpoint,
// it is considered alive.
//
// GET /health
// Response: {"code":0,"msg":"ok","data":{"status":"ok"}}
func (h *HealthHandler) Health(c *gin.Context) {
	response.Success(c, gin.H{"status": "ok"})
}

// Readiness checks whether all downstream dependencies are reachable.
// Returns 200 only when both MySQL and Redis are healthy; 503 otherwise.
// The load balancer should remove an instance from rotation when this returns 503.
//
// GET /readiness
// Response (all healthy):   {"code":0,"msg":"ok","data":{"mysql":"ok","redis":"ok"}}
// Response (degraded):      {"code":503001,"msg":"service unavailable","data":{"mysql":"ok","redis":"error"}}
func (h *HealthHandler) Readiness(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	checks := gin.H{}
	allOK := true

	// ── MySQL ping ────────────────────────────────────────────────────────────
	sqlDB, err := h.db.DB()
	if err != nil || sqlDB.PingContext(ctx) != nil {
		checks["mysql"] = "error"
		allOK = false
	} else {
		checks["mysql"] = "ok"
	}

	// ── Redis ping ────────────────────────────────────────────────────────────
	if _, err := h.rdb.Ping(ctx).Result(); err != nil {
		checks["redis"] = "error"
		allOK = false
	} else {
		checks["redis"] = "ok"
	}

	if allOK {
		response.Success(c, checks)
		return
	}

	// Return 503 so Nginx or k8s removes this instance from rotation.
	// Uses the standard response.Unavailable envelope so the structure is
	// identical to every other response in the API (Step 18 收口).
	response.Unavailable(c, checks)
}
