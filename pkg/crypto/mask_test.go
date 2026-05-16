package crypto

import "testing"

func TestMaskPhone(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"13800138000", "138****8000"},
		{"1234567", "123****4567"}, // len=7, last 4 = "4567"
		{"12345", "12345"},         // shorter than 7 — returned as-is
		{"", ""},
	}
	for _, c := range cases {
		got := MaskPhone(c.input)
		if got != c.want {
			t.Errorf("MaskPhone(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestMaskToken(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"abcdefgh12345", "abcdefgh..."}, // len > 8 → show first 8 + "..."
		{"short", "*****"},               // len <= 8 → fully masked
		{"12345678", "********"},         // len == 8 → fully masked
		{"", ""},
	}
	for _, c := range cases {
		got := MaskToken(c.input)
		if got != c.want {
			t.Errorf("MaskToken(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}
