package logger

import (
	"cooking-platform/pkg/crypto"

	"go.uber.org/zap"
)

// MaskedPhone returns a zap.Field that logs the phone number in masked form
// ("138****9876") under the key "phone_masked". Use this instead of
// zap.String("phone", phone) to prevent PII leakage in log files.
func MaskedPhone(phone string) zap.Field {
	return zap.String("phone_masked", crypto.MaskPhone(phone))
}

// MaskedToken returns a zap.Field that logs only the first 8 characters of a
// token under the key "token_prefix". Sufficient for log correlation without
// exposing the full credential.
func MaskedToken(token string) zap.Field {
	return zap.String("token_prefix", crypto.MaskToken(token))
}
