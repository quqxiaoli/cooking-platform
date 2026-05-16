package crypto

import "strings"

// MaskPhone masks a phone number as "138****9876".
// Returns the input unchanged if it is shorter than 7 characters.
func MaskPhone(phone string) string {
	if len(phone) < 7 {
		return phone
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

// MaskToken returns the first 8 characters of a token followed by "...",
// so log entries can be correlated without exposing the full credential.
func MaskToken(token string) string {
	if len(token) <= 8 {
		return strings.Repeat("*", len(token))
	}
	return token[:8] + "..."
}
