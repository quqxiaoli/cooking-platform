// Package sms — sender.go defines the SMS sending interface and the factory
// that selects the concrete implementation based on configuration.
package sms

import (
	"context"
	"fmt"

	"cooking-platform/pkg/config"
)

// Sender is the contract every SMS implementation must satisfy.
//
// SendCode dispatches a verification code to the given phone number.
// The phone format is assumed to be already validated by the caller
// (pkg/validator), so implementations may treat the input as trusted.
//
// Implementations must return a wrapped error on transport failures so the
// caller can decide whether to expose details (typically: log and return a
// generic 500 to the user).
type Sender interface {
	SendCode(ctx context.Context, phone string, code string) error
}

// NewSender constructs a Sender from configuration.
//
// Provider="mock" returns MockSender (logs the code instead of sending).
// Provider="aliyun" will be wired up in Step 10 — currently returns an error
// so misconfiguration fails loudly at startup rather than silently swallowing
// codes.
func NewSender(cfg config.SMSConfig) (Sender, error) {
	switch cfg.Provider {
	case "mock", "":
		return NewMockSender(), nil
	case "aliyun":
		return NewAliyunSender(cfg)
	default:
		return nil, fmt.Errorf("unknown sms provider: %q (supported: mock, aliyun)", cfg.Provider)
	}
}
