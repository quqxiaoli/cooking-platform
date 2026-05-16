// Package audit — mock.go is the development-only Auditor implementation.
// Verdict is controlled by cfg.Audit.MockResult (pass / suspect / reject).
// Default is "pass" so dev posts become visible immediately after the
// AuditConsumer goroutine processes the PostEvent.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cooking-platform/internal/model"
	"cooking-platform/pkg/config"
)

// MockAuditor returns a fixed verdict configured at startup.
type MockAuditor struct {
	result uint8 // model.AuditStatusMachinePass | Suspect | MachineDeny
}

// NewMockAuditor constructs a MockAuditor.
// cfg.MockResult: "pass" → 1, "suspect" → 2, "reject" → 4.
// Anything else (including empty) defaults to pass.
func NewMockAuditor(cfg config.AuditConfig) *MockAuditor {
	var status uint8
	switch cfg.MockResult {
	case "suspect":
		status = model.AuditStatusSuspect
	case "reject":
		status = model.AuditStatusMachineDeny
	default:
		status = model.AuditStatusMachinePass
	}
	return &MockAuditor{result: status}
}

// Review logs the mock decision and returns immediately.
func (a *MockAuditor) Review(_ context.Context, req ReviewRequest) (ReviewResult, error) {
	raw, _ := json.Marshal(map[string]interface{}{
		"provider":  "mock",
		"post_id":   req.PostID,
		"status":    a.result,
		"timestamp": time.Now().UnixMilli(),
	})
	remark := fmt.Sprintf("[MOCK] audit_status=%d post_id=%d", a.result, req.PostID)
	return ReviewResult{
		Status: a.result,
		Remark: remark,
		Raw:    string(raw),
	}, nil
}
