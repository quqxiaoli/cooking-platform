package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
)

// EncryptPhone encrypts a plaintext phone number with AES-256-GCM.
//
// Output format: StdBase64(nonce[12] || ciphertext || tag[16]).
// Total base64 length ≈ ceil((12 + len(plain) + 16) / 3) * 4 ≈ 52–60 chars
// for an 11-digit phone — well within the VARCHAR(200) column.
//
// When keyHex is empty, the plaintext is returned unchanged.
// This allows dev environments to run without configuring a key
// while keeping the code path identical to production.
func EncryptPhone(plaintext, keyHex string) (string, error) {
	if keyHex == "" {
		return plaintext, nil
	}

	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return "", fmt.Errorf("crypto: decode key hex: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("crypto: new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: new gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: gen nonce: %w", err)
	}

	// Seal appends ciphertext+tag to nonce in-place.
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptPhone decrypts a ciphertext produced by EncryptPhone.
//
// When keyHex is empty, the ciphertext is returned unchanged
// (mirrors the dev-mode no-op in EncryptPhone so round-trips stay consistent).
func DecryptPhone(ciphertext, keyHex string) (string, error) {
	if keyHex == "" {
		return ciphertext, nil
	}

	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return "", fmt.Errorf("crypto: decode key hex: %w", err)
	}

	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("crypto: base64 decode: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("crypto: new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: new gcm: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("crypto: ciphertext too short (%d bytes, need >%d)", len(data), nonceSize)
	}

	nonce, sealed := data[:nonceSize], data[nonceSize:]
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		// GCM authentication failure — wrong key or corrupted/plaintext data.
		return "", fmt.Errorf("crypto: gcm open: %w", err)
	}

	return string(plain), nil
}

// HashPhone returns hex(SHA256(phone + pepper)).
//
// When pepper is empty, the result is identical to SHA256(phone) — backward
// compatible with the phone_hash values written during Step 3–10 (dev setup
// where encryption.phone_pepper is unset). Production deployments should
// inject a non-empty pepper via APP_ENCRYPTION_PHONE_PEPPER to prevent
// rainbow-table attacks on a leaked users table.
func HashPhone(phone, pepper string) string {
	sum := sha256.Sum256([]byte(phone + pepper))
	return hex.EncodeToString(sum[:])
}
