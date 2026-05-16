// Package sms — aliyun.go is the production Sender implementation that
// dispatches verification codes via Aliyun dysmsapi (SMS service).
//
// The SMS API is synchronous: SendCode blocks until Aliyun confirms
// dispatch (or returns an error). The caller (UserService) already runs
// this in the HTTP request goroutine, which is acceptable since Aliyun
// typically responds in < 300 ms.
//
// Rate limiting is NOT done here — that lives in UserService (three-
// dimension protection: phone window / phone daily / IP daily).
//
// AccessKeyID / AccessKeySecret are never hardcoded; production injects
// them via APP_SMS_ACCESS_KEY_ID / APP_SMS_ACCESS_KEY_SECRET env vars
// (read by pkg/config, passed to NewAliyunSender via SMSConfig).
//
// Added in Step 10 (aliyun SMS real integration).
package sms

import (
	"context"
	"fmt"

	"cooking-platform/pkg/config"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/dysmsapi"
)

// AliyunSender dispatches SMS via Aliyun dysmsapi.
type AliyunSender struct {
	client       *dysmsapi.Client
	signName     string
	templateCode string
}

// NewAliyunSender constructs an AliyunSender.
// The dysmsapi client is created once at startup and is safe for concurrent use.
// Region is fixed to cn-hangzhou — Aliyun SMS is a global service accessible
// from any region endpoint; cn-hangzhou is the recommended default.
func NewAliyunSender(cfg config.SMSConfig) (*AliyunSender, error) {
	client, err := dysmsapi.NewClientWithAccessKey("cn-hangzhou", cfg.AccessKeyID, cfg.AccessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("new dysmsapi client: %w", err)
	}
	return &AliyunSender{
		client:       client,
		signName:     cfg.SignName,
		templateCode: cfg.TemplateCode,
	}, nil
}

// SendCode dispatches a verification code to the given phone number.
// TemplateParam uses the JSON format required by Aliyun: {"code":"XXXXXX"}.
// Aliyun returns Code="OK" on success; any other code is treated as an error
// so the caller can surface a generic "SMS failed, please retry" to the user.
func (s *AliyunSender) SendCode(_ context.Context, phone string, code string) error {
	req := dysmsapi.CreateSendSmsRequest()
	req.PhoneNumbers = phone
	req.SignName = s.signName
	req.TemplateCode = s.templateCode
	req.TemplateParam = fmt.Sprintf(`{"code":"%s"}`, code)

	resp, err := s.client.SendSms(req)
	if err != nil {
		return fmt.Errorf("dysmsapi SendSms: %w", err)
	}
	if resp.Code != "OK" {
		return fmt.Errorf("dysmsapi rejected: code=%s message=%s", resp.Code, resp.Message)
	}
	return nil
}
