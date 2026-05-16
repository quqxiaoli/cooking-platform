// Package audit — auditor.go defines the Auditor interface, shared types,
// and the NewAuditor factory that selects the concrete implementation.
package audit

import (
	"context"
	"fmt"

	"cooking-platform/internal/model"
	"cooking-platform/pkg/config"
)

// Auditor is the contract every content-moderation implementation must satisfy.
//
// Review submits a post's text + images for moderation and returns a verdict
// synchronously. AuditConsumer calls this from its subscription goroutine;
// the call may block for the duration of the API round-trip (~200-800 ms for
// Aliyun Green). Implementations must respect ctx cancellation.
type Auditor interface {
	Review(ctx context.Context, req ReviewRequest) (ReviewResult, error)
}

// ReviewRequest carries all reviewable content for one post.
// ImageURLs is the union of all step image URLs (LoadSteps flattens them).
// Empty ImageURLs means text-only review.
type ReviewRequest struct {
	PostID    int64
	AuthorID  int64
	Title     string
	Content   string
	ImageURLs []string
}

// ReviewResult is the normalised verdict returned by any Auditor implementation.
// Status maps directly to model.AuditStatusXxx constants (1/2/4 only —
// 3 and 5 are reserved for human review paths, never emitted by machine).
type ReviewResult struct {
	// Status is one of:
	//   model.AuditStatusMachinePass (1) — content is clean
	//   model.AuditStatusSuspect     (2) — borderline, queued for human review
	//   model.AuditStatusMachineDeny (4) — content violates policy
	Status uint8
	Remark string // human-readable summary of the verdict
	Raw    string // raw JSON response from the provider (written to audit_log)
}

// NewAuditor constructs an Auditor from configuration.
// Provider="mock" returns MockAuditor (verdict fixed by cfg.MockResult).
// Provider="aliyun" returns AliyunAuditor (calls Aliyun Green API).
func NewAuditor(cfg config.AuditConfig) (Auditor, error) {
	switch cfg.Provider {
	case "mock", "":
		return NewMockAuditor(cfg), nil
	case "aliyun":
		return NewAliyunAuditor(cfg)
	default:
		return nil, fmt.Errorf("unknown audit provider: %q (supported: mock, aliyun)", cfg.Provider)
	}
}

// worstStatus takes two machine-audit statuses and returns the more restrictive one.
// Severity order: MachineDeny(4) > Suspect(2) > MachinePass(1).
func worstStatus(a, b uint8) uint8 {
	rank := func(s uint8) int {
		switch s {
		case model.AuditStatusMachineDeny:
			return 3
		case model.AuditStatusSuspect:
			return 2
		default:
			return 1
		}
	}
	if rank(a) >= rank(b) {
		return a
	}
	return b
}
