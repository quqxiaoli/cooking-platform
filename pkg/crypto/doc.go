// Package crypto provides field-level encryption utilities.
// aes.go  — AES-256-GCM encrypt/decrypt for phone number storage (Step 11).
// mask.go — one-way SHA-256 hash of phone number for indexed lookup (Step 11).
// Design: store AES-GCM ciphertext in users.phone; store SHA-256 hash in
// users.phone_hash with a unique index. Login looks up by phone_hash,
// then decrypts phone to verify exact match.
package crypto
