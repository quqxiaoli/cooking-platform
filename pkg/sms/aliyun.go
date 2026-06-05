// Package sms — aliyun.go is the production Sender implementation backed by
// Aliyun 号码认证服务 (dypnsapi) 的 SendSmsVerifyCode 接口。
//
// 历史背景：早期实现走的是标准短信服务 (dysmsapi)，但个人账号在阿里云无法
// 申请到自定义签名/模板（公司资质要求），项目改为购买「短信认证服务」套餐，
// 使用赠送签名 + 赠送模板。详见 docs/maintenance/2026-06-02_sms_dypns_migration.md。
//
// 使用模式：自带验证码（mode B）。也就是说，验证码仍由 UserService 自己生成、
// 自己存 Redis、自己校验；本实现只把 dypns 当作「免审签名 + 发送通道」来用，
// 而不使用其 GenerateCode / CheckSmsVerifyCode 闭环能力。这样保留了现有
// service 层的限流和验证码生命周期逻辑，业务层零改动。
//
// 频控策略：dypns 默认按 (Sign+Template+Phone) 维度做 60s 频控，与 service
// 层 ratelimit.sms_phone_window 重复且会产生 phantom rejection（你的限流没限、
// 阿里云那边却拒绝）。因此显式传 Interval=1，把限流权完全交给 service 层，
// 保证「用户看到的限流原因 = 你 metrics 里的限流原因」。
package sms

import (
	"context"
	"fmt"
	"math"

	"cooking-platform/pkg/config"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/requests"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/dypnsapi"
)

// AliyunSender 调用 dypns SendSmsVerifyCode 下发验证码短信。
type AliyunSender struct {
	client       *dypnsapi.Client
	signName     string
	templateCode string
	validMinutes int // 模板变量 ${min} 的值，源自 cfg.CodeTTL 向上取整到分钟
	validSeconds int // ValidTime 参数，源自 cfg.CodeTTL；mode B 下 dypns 不存 code，仅作完整性传递
}

// NewAliyunSender 构造 AliyunSender。
// dypnsapi 客户端在启动时创建一次，并发安全。
// region 固定 cn-hangzhou —— dypns 是全局服务，从任意 region endpoint 都可访问。
func NewAliyunSender(cfg config.SMSConfig) (*AliyunSender, error) {
	client, err := dypnsapi.NewClientWithAccessKey("cn-hangzhou", cfg.AccessKeyID, cfg.AccessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("new dypnsapi client: %w", err)
	}

	minutes := int(math.Ceil(cfg.CodeTTL.Minutes()))
	if minutes < 1 {
		minutes = 1
	}
	seconds := int(cfg.CodeTTL.Seconds())
	if seconds < 1 {
		seconds = 1
	}

	return &AliyunSender{
		client:       client,
		signName:     cfg.SignName,
		templateCode: cfg.TemplateCode,
		validMinutes: minutes,
		validSeconds: seconds,
	}, nil
}

// SendCode 把验证码 code 下发到 phone。
//
// TemplateParam 走「自带 code」模式：{"code":"<code>","min":"<minutes>"}。
// 文档说明 mode B 下 dypns 不会校验该 code（这正是我们要的，校验由 service 层做）。
//
// 阿里云返回 Code="OK" 视为成功；其他状态码（含 Success=false 且 Code="OK" 的
// 异常情况）一律包装成错误，调用方据此 DeleteSMSCode + 返回 5xx。
func (s *AliyunSender) SendCode(_ context.Context, phone string, code string) error {
	req := dypnsapi.CreateSendSmsVerifyCodeRequest()
	req.PhoneNumber = phone
	req.SignName = s.signName
	req.TemplateCode = s.templateCode
	req.TemplateParam = buildTemplateParam(code, s.validMinutes)
	req.ValidTime = requests.NewInteger(s.validSeconds)
	req.Interval = requests.NewInteger(1) // 见 package 注释：让 service 层独占限流权

	resp, err := s.client.SendSmsVerifyCode(req)
	if err != nil {
		return fmt.Errorf("dypnsapi SendSmsVerifyCode: %w", err)
	}
	if resp.Code != "OK" || !resp.Success {
		return fmt.Errorf("dypnsapi rejected: code=%s message=%s", resp.Code, resp.Message)
	}
	return nil
}

// buildTemplateParam 拼接 dypns TemplateParam。
// 单独抽出来便于单测 —— 字符串拼错会让模板渲染失败但接口仍返 OK，
// 导致用户收到「您的验证码是${code}」这种没渲染的鬼东西。
func buildTemplateParam(code string, minutes int) string {
	return fmt.Sprintf(`{"code":"%s","min":"%d"}`, code, minutes)
}
