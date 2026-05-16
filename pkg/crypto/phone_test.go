package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// testKey is a valid 32-byte AES-256 key expressed as 64 hex chars.
const testKey = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

func TestEncryptDecryptRoundTrip(t *testing.T) {
	phone := "13800138000"
	ciphertext, err := EncryptPhone(phone, testKey)
	if err != nil {
		t.Fatalf("EncryptPhone: %v", err)
	}
	if ciphertext == phone {
		t.Fatal("ciphertext must not equal plaintext")
	}

	got, err := DecryptPhone(ciphertext, testKey)
	if err != nil {
		t.Fatalf("DecryptPhone: %v", err)
	}
	if got != phone {
		t.Fatalf("round-trip: got %q want %q", got, phone)
	}
}

func TestEncryptProducesDistinctCiphertexts(t *testing.T) {
	// Each call generates a fresh random nonce → two encryptions of the same
	// phone must produce different ciphertexts (IND-CPA property of GCM).
	phone := "13800138000"
	c1, _ := EncryptPhone(phone, testKey)
	c2, _ := EncryptPhone(phone, testKey)
	if c1 == c2 {
		t.Fatal("two encryptions of the same phone produced identical ciphertexts (nonce reuse?)")
	}
}

func TestEncryptEmptyKeyReturnsPlaintext(t *testing.T) {
	phone := "13900000000"
	got, err := EncryptPhone(phone, "")
	if err != nil {
		t.Fatalf("EncryptPhone(key=\"\"): %v", err)
	}
	if got != phone {
		t.Fatalf("dev mode: got %q want plaintext %q", got, phone)
	}
}

func TestDecryptEmptyKeyReturnsInputUnchanged(t *testing.T) {
	phone := "13900000000"
	got, err := DecryptPhone(phone, "")
	if err != nil {
		t.Fatalf("DecryptPhone(key=\"\"): %v", err)
	}
	if got != phone {
		t.Fatalf("dev mode: got %q want %q", got, phone)
	}
}

func TestDecryptRejectsPlaintextWhenKeySet(t *testing.T) {
	// A plaintext phone number cannot pass AES-GCM authentication.
	// This is the property the migration script relies on to detect
	// "not yet encrypted" rows.
	_, err := DecryptPhone("13800138000", testKey)
	if err == nil {
		t.Fatal("expected error decrypting plaintext phone with a real key")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	ciphertext, _ := EncryptPhone("13800138000", testKey)
	wrongKey := strings.Repeat("ff", 32) // valid hex, wrong key
	_, err := DecryptPhone(ciphertext, wrongKey)
	if err == nil {
		t.Fatal("expected GCM authentication failure with wrong key")
	}
}

func TestHashPhoneNoPepperEqualsPlainSHA256(t *testing.T) {
	phone := "13800138000"
	sum := sha256.Sum256([]byte(phone))
	expected := hex.EncodeToString(sum[:])

	got := HashPhone(phone, "")
	if got != expected {
		t.Fatalf("HashPhone(pepper=\"\") = %q, want %q", got, expected)
	}
}

func TestHashPhoneWithPepperDiffersFromNoPepper(t *testing.T) {
	phone := "13800138000"
	noPepper := HashPhone(phone, "")
	withPepper := HashPhone(phone, "deployment-secret-pepper")
	if noPepper == withPepper {
		t.Fatal("pepper must change the hash output")
	}
}

func TestHashPhoneIsDeterministic(t *testing.T) {
	phone := "13800138000"
	pepper := "my-pepper"
	h1 := HashPhone(phone, pepper)
	h2 := HashPhone(phone, pepper)
	if h1 != h2 {
		t.Fatalf("HashPhone is non-deterministic: %q vs %q", h1, h2)
	}
}

func TestHashPhoneOutputIsLower64HexChars(t *testing.T) {
	h := HashPhone("13800138000", "")
	if len(h) != 64 {
		t.Fatalf("expected 64 hex chars, got %d: %q", len(h), h)
	}
	if h != strings.ToLower(h) {
		t.Fatalf("expected lowercase hex, got %q", h)
	}
}
