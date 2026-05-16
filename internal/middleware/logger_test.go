package middleware

import "testing"

func TestSanitizeQuery(t *testing.T) {
	cases := []struct {
		name  string
		input string
		noStr string // must NOT appear in output
		okStr string // must appear in output (empty = skip)
	}{
		{
			name:  "phone param redacted",
			input: "phone=13800138000&page=1",
			noStr: "13800138000",
			okStr: "page=1",
		},
		{
			name:  "code param redacted",
			input: "code=123456",
			noStr: "123456",
		},
		{
			name:  "token param redacted",
			input: "token=eyJhbGciOiJIUzI1NiJ9",
			noStr: "eyJhbGciOiJIUzI1NiJ9",
		},
		{
			name:  "non-sensitive params preserved",
			input: "keyword=chicken&page=2",
			okStr: "chicken",
		},
		{
			name:  "empty input",
			input: "",
			okStr: "",
		},
		{
			name:  "phone with mixed case key",
			input: "Phone=13800138000",
			noStr: "13800138000",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeQuery(c.input)
			if c.noStr != "" && contains(got, c.noStr) {
				t.Errorf("sanitizeQuery(%q) = %q — should NOT contain %q", c.input, got, c.noStr)
			}
			if c.okStr != "" && !contains(got, c.okStr) {
				t.Errorf("sanitizeQuery(%q) = %q — should contain %q", c.input, got, c.okStr)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}
