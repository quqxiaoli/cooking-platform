# 短信通道改造：dysms → dypns 自带 code 模式

> **日期**：2026-06-02
> **触发**：个人账号在阿里云无法申请到自定义签名/模板，原 dysmsapi 实现在 prod 不可用。
> **影响范围**：`pkg/sms/aliyun.go` 重写；`configs/config.{,prod}.yaml` 注释/取值更新；业务层零改动。
> **类型**：本项目收官后的首次维护型改造（不进入 v2 范畴）。

---

## 1. 背景

CLAUDE.md §1 已声明项目正式收官，进入维护期。本次改造属于「触发型维护」，
不在 §8.2 季度计划内，由 prod 上线后接入真实短信通道时发现的资质问题驱动。

原始实现（`pkg/sms/aliyun.go`，Step 10 落地）走的是阿里云**标准短信服务 dysmsapi**，
依赖自定义签名 + 自定义模板。但：

- 阿里云签名审批要求企业资质（营业执照），**个人账号不支持自定义签名**
- 阿里云控制台「添加签名」页面已主动提示：「个人客户若无企业资质，推荐使用免资质签名模板申请的短信认证产品」

因此 dysmsapi 路径在个人账号下走不通，必须改方案。

## 2. 决策

| 候选方案 | 评估结果 |
| --- | --- |
| A. 注册个体工商户 → 走 dysms 自定义签名 | ✗ 投入产出比过低（≈500 元 + 数周流程，只为发短信） |
| B. 换腾讯云/华为云 | ✗ 同样卡在「个人 vs 企业资质」，且要新写 sender |
| **C. 阿里云号码认证服务 dypns 套餐**（短信认证，自带 code 模式） | **✓ 采用** |

采用方案 C 的理由：

- 阿里云官方为个人开发者准备的产品，**免审签名/模板**，开箱即用
- 提供「自带 code」模式（`TemplateParam: {"code":"123456","min":"5"}`），允许复用现有
  service 层的验证码生成 + Redis 存储 + 自校验逻辑 → **业务层零改动**
- 复用现有 `pkg/sms/` 接口抽象与 `Sender.SendCode(ctx, phone, code)` 签名
- SDK 已在 `go.mod` 主 module `alibaba-cloud-sdk-go v1.63.107` 内，无需新增依赖

## 3. 与 PRD 的偏离点

按 CLAUDE.md §6.3 偏离点登记机制：

| PRD 章节 / 规则 | 实现差异 | 选择此方案的原因 |
| --- | --- | --- |
| PRD §3 技术栈：「短信：阿里云短信（生产）」 | 改为「阿里云号码认证服务 dypns（生产）」 | 个人账号资质限制，dysms 自定义签名走不通 |
| PRD §8 USER-03 三维限流（service 层） | 保留不变；额外把 dypns 默认 60s 频控通过 `Interval=1` 关掉 | 双重限流会导致 phantom rejection：你的 Redis 未限但阿里云拒绝，用户困惑、可观测性差。让 service 层独占限流权 |
| PRD 隐含：`TemplateParam={"code":"X"}` 单变量 | 改为 `{"code":"X","min":"5"}` 双变量 | dypns 赠送模板 `100001` 模板内容含 `${code}` 和 `${min}` 两个占位符，必须同时传 |

## 4. 实施细节

### 4.1 代码改动

- `pkg/sms/aliyun.go`：原地重写，从 `dysmsapi.Client` 改为 `dypnsapi.Client`，
  接口签名 `Sender.SendCode(ctx, phone, code)` 保持不变。
- `pkg/sms/sender.go`：**不动**，工厂仍按 `provider: aliyun` 选择 AliyunSender，
  字面值不重命名以避免 prod env / yaml / validator 三处联动改动。
- `pkg/config/config.go`：**不动**，SMSConfig 7 个字段语义在 dypns 下仍适用：
  `sign_name` → 赠送签名名称，`template_code` → 赠送模板 Code，
  `code_ttl` → 同时驱动模板 `${min}` 变量（`ceil(code_ttl / 60s)`）。
- `internal/service/user_service.go`：**不动**，验证码生成/Redis/限流/校验逻辑零改动。
- `pkg/sms/mock.go`：**不动**。
- `internal/cache/`：**不动**，验证码 Redis key 仍由 service 层维护。

### 4.2 配置改动（三 yaml）

- `configs/config.yaml`（dev）：注释更新，指向本文档；字段值不变（仍 `provider: mock`）。
- `configs/config.docker.yaml`（dev docker）：未变动，仅依赖默认值，与 dev 行为一致。
- `configs/config.prod.yaml`（prod）：
  - `provider: mock` → `provider: aliyun`
  - `sign_name: "速通互联验证码"`（dypns 赠送签名 5 选 1）
  - `template_code: "100001"`（登录/注册模板）
  - 注释新增 RAM 子账号需授权 `AliyunDypnsFullAccess` 的提醒

### 4.3 dypns 调用契约

| 参数 | 取值 | 说明 |
| --- | --- | --- |
| `PhoneNumber` | 调用方传入 | 已由 validator 校验 |
| `SignName` | `cfg.SignName`（来自 yaml） | 必须用赠送签名 |
| `TemplateCode` | `cfg.TemplateCode`（来自 yaml） | 必须用赠送模板 |
| `TemplateParam` | `{"code":"<code>","min":"<minutes>"}` | minutes = ceil(code_ttl / 60s) |
| `ValidTime` | `cfg.CodeTTL.Seconds()` | mode B 下 dypns 不存 code，仅完整性传递 |
| `Interval` | `1` | 关闭 dypns 60s 频控，service 层独占限流 |
| 其他 | 不传 | CodeLength/CodeType/DuplicatePolicy 是 mode A 参数 |

### 4.4 凭证

- 复用现有 `APP_SMS_ACCESS_KEY_ID` / `APP_SMS_ACCESS_KEY_SECRET` env var
- RAM 子账号需**新增**授权策略：`AliyunDypnsFullAccess`
  （原 `AliyunDysmsFullAccess` 可以保留也可以撤销 —— 不再使用 dysms，
  从最小权限原则建议撤销）

## 5. 回滚方案

如果 dypns 套餐故障或被用尽（100 条/月），临时回滚步骤：

1. **服务降级到 mock**：`configs/config.prod.yaml` 改 `provider: mock`，
   `force-recreate` 重启 app —— 此时验证码只写日志不真发，**用户无法登录**。
   仅适合「停服公告 + 配合人工发码」的紧急场景。
2. **永久回滚到 dysms**：需要先解决个人账号签名审批问题（注册企业 / 借用资质），
   然后 `git revert` 本次 commit 即可恢复 dysmsapi 实现。

## 6. 验证清单（上线后必做）

2026-06-02 15:41–15:44 在 prod (47.238.29.251) 用真手机号 17596509062 完成端到端验证：

- [x] 真手机号请求 `/api/v1/auth/send-code`，5 分钟内收到验证码（109341）
- [x] 短信内容包含正确的 6 位 code 和 `5` 分钟有效期（模板变量 `${code}` / `${min}` 都正确渲染）
- [x] 短信签名显示为「【速通互联验证码】」
- [x] 验证码可成功登录（service 层校验路径正确，user id=21 拿到 access + refresh token）
- [x] 阿里云控制台「短信认证 - 发送记录」可见对应记录，套餐余量 100 → 98（首次成功 + 一次窗口外重测，各 -1）
- [x] 同一手机号 60s 内重发，返回 service 层限流 `HTTP 429 / errcode 410105`，**不**触达阿里云
- [x] 日志（`/var/log/cooking/app.log`）无 `dypnsapi rejected` 错误

## 7. 后续运维 SOP 补充

- **套餐余量监控**：当前 100 条套餐够测试 + 小量上线；真实流量后需在
  阿里云控制台开启「套餐余量预警」（短信/邮件通知阈值，建议 20%）
- **AK/SK 轮换**：参照 §8.1 月度巡检流程，单独检查 SMS RAM 子账号是否被滥用

## 8. 关联文档

- 接口文档：阿里云 `SendSmsVerifyCode` API（dypns 2017-05-25）
- 实现文件：`pkg/sms/aliyun.go`
- 配置文件：`configs/config.prod.yaml` §sms
- 红线遵守：CLAUDE.md §10 红线 #2（prod 配置改动走 git → push → 服务器 pull → recreate）
