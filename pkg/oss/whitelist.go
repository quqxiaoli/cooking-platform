// Package oss — whitelist.go enforces "image URLs we accept into the
// database must originate from our own OSS host".
//
// This is defence in depth on top of the presign+nonce flow:
//   - Even if a client somehow forges a callback nonce, the URL it tries
//     to attach to a profile / post must still pass IsAllowedURL.
//   - Forbids a category of attacks where a compromised frontend points
//     avatars / covers at hostile third-party hosts (tracking pixels,
//     malware payloads, IP-disclosure beacons).
//
// The check is intentionally string-prefix based — cheap, predictable,
// and trivially auditable. URL parsing nuances (case-insensitive host,
// trailing slash) are normalised once in the helpers below.
package oss

import (
	"strings"
)

// IsAllowedURL reports whether u is non-empty AND begins with prefix.
//
// Empty u is allowed — service layers treat it as "clear this field"
// (e.g. UpdateProfile with avatar_url = "" clears the avatar).
//
// Comparison is case-sensitive on the path portion but case-insensitive on
// the scheme + host (URLs are case-sensitive in path per RFC 3986, but
// hosts are not). To keep this helper boringly correct, we lowercase both
// strings before the prefix check. Callers must therefore configure
// URLPrefix in lower case — config.validate enforces this.
func IsAllowedURL(u, prefix string) bool {
	if u == "" {
		return true
	}
	if prefix == "" {
		// Misconfiguration: refuse rather than silently allowing everything.
		return false
	}
	return strings.HasPrefix(strings.ToLower(u), strings.ToLower(prefix))
}

// AllAllowed reports whether every URL in urls passes IsAllowedURL.
// Convenience for post step image lists.
func AllAllowed(urls []string, prefix string) bool {
	for _, u := range urls {
		if !IsAllowedURL(u, prefix) {
			return false
		}
	}
	return true
}
