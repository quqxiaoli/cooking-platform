package sms

import "testing"

func TestBuildTemplateParam(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		minutes int
		want    string
	}{
		{"6 位数字 + 5 分钟", "123456", 5, `{"code":"123456","min":"5"}`},
		{"4 位数字 + 1 分钟", "0000", 1, `{"code":"0000","min":"1"}`},
		{"8 位数字 + 10 分钟", "12345678", 10, `{"code":"12345678","min":"10"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildTemplateParam(c.code, c.minutes)
			if got != c.want {
				t.Errorf("buildTemplateParam(%q, %d) = %q, want %q", c.code, c.minutes, got, c.want)
			}
		})
	}
}
