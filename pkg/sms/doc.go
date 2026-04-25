// Package sms wraps the Aliyun SMS SDK for sending verification codes.
// The interface is thin: SendCode(phone, code string) error.
// Rate limiting (1 SMS per 60s, max 5 per day) is enforced in cache/sms.go,
// not here — this package is pure I/O.
// Added in Step 3 (user module) when phone verification is wired up.
package sms
