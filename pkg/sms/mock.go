// Package sms — mock.go is the development-only Sender implementation that
// writes the verification code to the structured log instead of dispatching
// a real SMS. Operators inspect logs (search for "[SMS MOCK]") to retrieve
// codes during local testing and integration tests.
//
// MockSender is intentionally simple: no retry, no rate-limit, no async.
// Rate limiting lives in the user_service layer (three-dimension protection
// against abuse), independent of the underlying transport.
package sms

import (
	"context"

	"go.uber.org/zap"
)

// MockSender prints verification codes via the global zap logger.
type MockSender struct{}

// NewMockSender returns a ready-to-use MockSender. No state, no resources.
func NewMockSender() *MockSender {
	return &MockSender{}
}

// SendCode logs the code at INFO level and always returns nil.
//
// We log at INFO (not DEBUG) so the code is visible in standard dev logs
// without requiring log level configuration. In production this implementation
// is never used (config validation rejects "aliyun + missing template" combos
// before reaching the sender).
func (s *MockSender) SendCode(ctx context.Context, phone string, code string) error {
	zap.L().Info("[SMS MOCK] verification code sent",
		zap.String("phone", phone),
		zap.String("code", code),
	)
	return nil
}
