# 后端 deploy follow-up · 执行结果回传

> **受众**:前端 `cooking-frontend` 项目的 Claude Code(独立 session,无法直接对话)。
> **时间**:2026-05-31(执行 `docs/handoff/backend-deploy-followup.md` §1-3 全过程)
> **状态**:🟢 全部完工 + 验证通过。前端可放心进入 Phase 1 smoke。
>
> 前端 session 启动后 `read` 本文件 → 全部上下文即可秒接。

---

## 0. TL;DR

| 项 | 结果 |
|---|---|
| nginx server 拆分(api. 后端 / mellowck. 前端) | ✅ 已 force-recreate 落地 |
| `api.mellowck.com` TLS 证书 | ✅ 已签,有效期至 2026-08-29 |
| 后端 CORS allow-list | ✅ 已替换占位符为 mellowck.com / www.mellowck.com,**镜像已 rebuild**(配置是 COPY 进镜像,需 rebuild) |
| `mellowck.com/api/v1/*` → `api.*` 301 兜底 | ✅ 已生效 |
| 主域 / + www. → 前端 Next.js | ✅ HTTP 200 |
| api-spec base URL 文档同步 | ✅ 已 commit |

**前端这边可以开始 Phase 1 smoke 了。**

---

## 1. 硬约束(回顾前端容器对齐情况)

前端容器已起来,以下三条约束**实测对齐**:

| 约束 | 期望值 | 实测 |
|---|---|---|
| docker network(external) | `cooking-platform_cooking-prod-net` | ✅ 一致 |
| container_name / service 名 | `cooking-frontend`(nginx server B 写死) | ✅ 一致 |
| 监听端口 | 3000(容器内,不需 publish) | ✅ `wget http://cooking-frontend:3000/` 从 nginx 容器内返 200 |

nginx server B 已通过 `proxy_pass http://cooking-frontend:3000` 接到前端,任何动这三条之一(改名 / 改端口 / 换 network)都会让 nginx 502,改之前提前打招呼。

---

## 2. §7 全套验证输出(实测)

执行时间:2026-05-31 13:17 UTC+8

```text
=== 1. https://api.mellowck.com/health ===
HTTP 200

=== 2. https://api.mellowck.com/api/v1/feed?size=1 ===
HTTP 200

=== 3. 主域 /api/v1/* 走 301 兜底 ===
HTTP 301 -> https://api.mellowck.com/api/v1/feed?size=1

=== 4a. CORS OPTIONS, Origin=https://mellowck.com(白名单内) ===
HTTP/2 204
access-control-allow-headers: Origin, Content-Type, Authorization, X-Request-ID
access-control-allow-methods: GET, POST, PUT, PATCH, DELETE, OPTIONS
access-control-allow-origin: https://mellowck.com
access-control-expose-headers: X-Request-ID
vary: Origin
x-request-id: c54821ba-dd69-45bd-abe4-b03f047a9c27

=== 4b. CORS, Origin=https://www.mellowck.com(白名单内) ===
HTTP/2 204
access-control-allow-origin: https://www.mellowck.com

=== 4c. CORS, Origin=https://evil.example.com(不在白名单) ===
HTTP/2 204
(无 access-control-allow-origin 头,浏览器会拒绝)

=== 5. docker network 名 ===
cooking-platform_cooking-prod-net

=== 6. https://api.mellowck.com/metrics(红线 §10.3) ===
HTTP 404

=== 7. https://mellowck.com/ ===
HTTP 200  Content-Type: text/html; charset=utf-8(Next.js)

=== 8. https://www.mellowck.com/ ===
HTTP 200  Content-Type: text/html; charset=utf-8

=== 9. 阶段 1 ACME 路径 ===
http://api.mellowck.com/.well-known/acme-challenge/probe → HTTP 404(预期,无文件)
```

---

## 3. ⚠️ 一个行为变化:`mellowck.com/health` 现在 404

旧行为(阶段 1 之前):`https://mellowck.com/health` → 后端 → HTTP 200。
新行为(阶段 2 之后):`https://mellowck.com/health` → 前端 Next.js → HTTP 404(Next 没这路由)。

**这是设计预期**——后端健康检查统一走 `https://api.mellowck.com/health`(实测 200)。

但如果前端这边在任何监控 / blackbox / 外部 cron / 老外链里配了 `https://mellowck.com/health` 当探针,**请切到 `https://api.mellowck.com/health`**。

---

## 4. 实施过程(供前端理解风险点)

按 `docs/handoff/backend-deploy-followup.md` §3 顺序两阶段:

### 阶段 1(commit `eedd5e6`)
- 改 `nginx.conf` :80 server_name 加 `api.mellowck.com`
- force-recreate nginx(✅ sha 一致,inode 坑没踩)
- 用 certbot webroot 签 api. 证书(CN + SAN = api.mellowck.com, expires 2026-08-29)

### 阶段 2(commit `f3c73fb`)
- 改 `nginx.conf` :443 拆 server A(api.→后端)/ B(mellowck.+www.→前端 + /api/v1 301 兜底)
- 改 `configs/config.prod.yaml` CORS allowed_origins
- 改 `docs/api-spec/00-overview.md` base URL → `api.mellowck.com`

### CORS 修复(过程中发现)
- 第一次 force-recreate app1/app2 后,CORS Allow-Origin 头**没回**
- 根因:Dockerfile 用 `COPY configs/`,配置是 build 时烤进镜像,普通 `--force-recreate` 不会 rebuild image,仍跑旧版
- 修复:`docker compose ... up -d --build --force-recreate app1 app2` 强制 rebuild,新 yaml 进镜像
- **后续每次改 prod yaml 都需 `--build` 否则不生效**——这是后端这边要记的坑,前端无关

---

## 5. 🟡 仍待补:certbot deploy-hook(不阻塞前端)

`certbot renew` 默认不会通知 nginx 重载新证书。当前是否已配 hook 未知。后端会单独 follow-up 检查:
```bash
cat /etc/letsencrypt/renewal-hooks/deploy/*.sh
crontab -l | grep -i certbot
systemctl list-timers | grep -i certbot
```
并按需补:
```sh
# /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh
#!/bin/sh
docker exec cooking-nginx-prod nginx -s reload
```
(cert 是目录挂载、inode 不变,reload 在这里是有效的——区别于 nginx.conf 单文件挂载)

期限:api. 证书 2026-08-29 到期。前端无关。

---

## 6. 回滚

阶段 1+2 改动都已 commit + push,要回滚:
1. `cp deploy/nginx/nginx.conf{.bak,}` (服务器上备份还在)
2. `git revert f3c73fb eedd5e6` (本地回滚 commits)
3. `docker compose ... up -d --build --force-recreate nginx app1 app2`

但**不要轻易回滚**——证书已签(不可撤回也不影响)、前端容器已起、CORS 已对齐。回滚等于把前端入口又切回 404。

---

## 7. 后端这边干完了,前端要不要做什么?

按 `docs/handoff/backend-deploy-followup.md` 规划,前端 next step:
- Phase 1 smoke(本计划 `PLAN_DEPLOY.md` §3.5 之后的步骤)
- 任何挂的,回头一起排
- smoke 通过后,前端在自己的 DEBT_LEDGER 加 D-009 / D-010 跟踪"deploy 后端 follow-up 已落"

后端这边的 followup 文档 `docs/handoff/backend-deploy-followup.md` 不动了——本输出文件就是它的执行回执。
