# OPS RUNBOOK · 前端运维速查

> **受众**:未来的我(或临时接手的人)。半夜 3 点被叫醒能照着排。
>
> **范围**:前端 + 接口层(BFF / 域名 / CORS / 部署)。**不**含后端栈内部(MySQL / Redis / RabbitMQ / Prometheus 等),那部分让后端写对等的 runbook。
>
> **关联文档**:全量计划 `docs/plans/PLAN_DEPLOY.md`(长,planning 视角);执行回执 `docs/handoff/backend-deploy-output.md`(后端阶段 2 落地证据);本文是浓缩版,出事时先翻这里。

---

## 1. TL;DR(域名 → 服务一行映射)

| 域名 | 落到 | TLS 到期 |
|---|---|---|
| `https://mellowck.com` | 前端 Next.js 容器 `cooking-frontend:3000` | 2026-08-29 |
| `https://www.mellowck.com` | 同上 | 同上 |
| `https://mellowck.com/api/v1/*` | 301 → `https://api.mellowck.com/$1` | — |
| `https://api.mellowck.com` | 后端 Go `cooking_backend`(app1+app2 upstream) | 2026-08-29 |
| `https://api.mellowck.com/health` | 后端 health 端点 | — |
| `https://api.mellowck.com/metrics` | 故意 404(红线 §10.3) | — |

**注**:`mellowck.com/health` **不存在**(Next.js 没这路由);健康探针必须打 `api.mellowck.com/health`。

---

## 2. 拓扑

```
                ┌───────────────┐
公网 443 ──────▶│ cooking-nginx │──┬─ server A: api.mellowck.com    ──▶ cooking_backend(app1+app2:8080)
                │     -prod     │  │
                │ (nginx:1.27)  │  ├─ server B: mellowck.com /      ──▶ cooking-frontend:3000
                └───────────────┘  │             www.mellowck.com /
                                   │
                                   └─ server B: */api/v1/*         301 ──▶ api.mellowck.com/api/v1/*

      docker network: cooking-platform_cooking-prod-net (external)
      ┌────────────────────────────────────────────────────────────────┐
      │ cooking-nginx-prod   cooking-frontend   cooking-app1-prod      │
      │ cooking-app2-prod    cooking-mysql-prod cooking-redis-*-prod   │
      │ cooking-rabbitmq-prod ...(后端栈细节)                          │
      └────────────────────────────────────────────────────────────────┘

      前端 BFF 路径(浏览器拿不到 token,所有后端调用走 BFF):
      浏览器 ──▶ mellowck.com/api/* ──▶ Next.js Route Handler ──▶ http://cooking-app1-prod:8080
                                          (BFF, 同 network 直连, 不走 nginx)
```

**为什么 BFF 直连 app1 不走 nginx?** 同网内直连免一次 TLS + nginx hop;代价是 app1 挂时 app2 顶不上(MVP 流量小可接受,Phase 2 评估)。

---

## 3. 速查表

### 3.1 前端容器

| 项 | 值 |
|---|---|
| ECS | `root@47.238.29.251`(阿里云 HK,SSH key 已配) |
| ECS 路径 | `/opt/cooking-frontend/`(`docker-compose.frontend.yml` + `.env.production`) |
| 容器名 | `cooking-frontend` |
| 镜像 | `cooking-frontend:<git-sha>` + `cooking-frontend:latest`(都 load 上去) |
| 端口 | 容器内 3000,**不映射宿主**(`expose: ["3000"]` only) |
| network | `cooking-platform_cooking-prod-net`(后端创建,前端 external join) |
| 用户 | `nextjs` (uid 1001),非 root |
| 启动命令 | `node server.js`(standalone 模式) |
| 日志 | json-file,10MB × 3(防爆盘) |

### 3.2 关键环境变量(`.env.production`)

| 变量 | 用途 | 谁读 |
|---|---|---|
| `BACKEND_URL=http://cooking-app1-prod:8080` | BFF 目标 | runtime |
| `NEXT_PUBLIC_APP_ENV=production` | 烤进 bundle,Sentry env tag | build + runtime |
| `SENTRY_DSN` | 服务端 SDK | runtime |
| `NEXT_PUBLIC_SENTRY_DSN` | 浏览器 SDK | build |
| `SENTRY_ORG` / `SENTRY_PROJECT` / `SENTRY_AUTH_TOKEN` | sourcemap 上传 | **仅** build-time |
| `SENTRY_RELEASE` | release 关联 commit | build-time,**deploy.sh 自动注 git SHA**,文件留空 |

文件权限必须 600。**不**入 git(.gitignore 有 `.env*` + `!.env.production.template`)。

---

## 4. 部署

### 4.1 改前端代码 → redeploy

```bash
cd ~/Desktop/cooking-frontend
git status                          # 确认 clean
git pull                            # 同步
source .env.production              # 导 Sentry 凭证
./deploy.sh                         # 全自动:build → save|ssh|load → scp → up -d → 等 healthy
```

`deploy.sh` 5 步,任一非 0 立刻停。日志 tee 到 stdout,失败时翻屏看哪步炸。

镜像传完后 prod 端 `--pull never`(本地没 registry),source 也不在 prod 上——改代码必须本地 build → 重传。

### 4.2 改后端时前端要做什么

| 后端改了 | 前端动作 |
|---|---|
| 后端容器名(`cooking-app1-prod`) | 改 `.env.production` 的 `BACKEND_URL`,redeploy |
| 后端 network 名 | 改 `docker-compose.frontend.yml` `networks:` block + scp + `docker compose up -d`(不用 rebuild 镜像) |
| 后端 API 契约 | 同步 `docs/api-spec/*`,改前端 schema / fetcher / types,redeploy |
| 后端 CORS 白名单 | 后端事 → 后端 redeploy(`--build --force-recreate`,configs 烤进镜像) |
| nginx server 块 | 后端事 → 后端 `--force-recreate nginx`(单文件 bind mount,reload 无效) |

---

## 5. 故障排查

### 5.1 `mellowck.com` 502

```bash
# 1. 前端容器是否在跑 + healthy
ssh root@47.238.29.251 'docker ps --filter name=cooking-frontend --format "{{.Names}}\t{{.Status}}"'

# 2. nginx 容器能否解析到前端
ssh root@47.238.29.251 'docker exec cooking-nginx-prod wget -qO- http://cooking-frontend:3000/api/health'
# 期望:{"ok":true}
# 失败 → network 名不一致 / 容器名漂了 / 前端容器死

# 3. 看前端容器日志
ssh root@47.238.29.251 'docker logs --tail 100 cooking-frontend'

# 4. 看 nginx 错误
ssh root@47.238.29.251 'docker exec cooking-nginx-prod cat /var/log/nginx/error.log | tail -30'
```

最常见原因:前端容器死了 / network 名漂移。

### 5.2 浏览器报 CORS

浏览器 console 看 origin 是不是 `https://mellowck.com` 或 `https://www.mellowck.com`(后端白名单)。其他 origin → 后端拒。

```bash
# 模拟浏览器 preflight
curl -i -X OPTIONS https://api.mellowck.com/api/v1/feed \
  -H 'Origin: https://mellowck.com' \
  -H 'Access-Control-Request-Method: GET'
# 期望:204 + access-control-allow-origin: https://mellowck.com
```

无 `access-control-allow-origin` → 后端 `configs/config.prod.yaml` 白名单错。**注**:后端改 yaml 必须 `--build --force-recreate`(configs 烤进镜像)。

### 5.3 证书过期 / api. 不通

```bash
echo | openssl s_client -servername api.mellowck.com -connect api.mellowck.com:443 2>/dev/null \
  | openssl x509 -noout -dates
# 期望:notAfter=Aug 29 ... 2026
```

到期前 30 天后端 certbot 应自动续。如果续了 nginx 没 reload → 后端 deploy-hook 没配(后端待办,不阻塞前端)。手动:
```bash
ssh root@47.238.29.251 'docker exec cooking-nginx-prod nginx -s reload'
```

### 5.4 容器一直 `(health: starting)` / 反复重启

90% 是 HEALTHCHECK URL 写 `localhost` 撞 IPv6 坑(busybox wget 偏 `[::1]`,Next standalone 只绑 v4)。Dockerfile HEALTHCHECK 必须 `http://127.0.0.1:3000/api/health`。

```bash
# 直接复现
ssh root@47.238.29.251 'docker exec cooking-frontend wget -qO- http://127.0.0.1:3000/api/health'
# 200 → 容器其实是好的,只是 HEALTHCHECK URL 错
```

其他可能:Next.js cold start 没起来 / `BACKEND_URL` 写错触发 BFF 启动报错 / OOM。

### 5.5 Sentry 没收到事件

1. `.env.production` 的 `SENTRY_DSN` / `NEXT_PUBLIC_SENTRY_DSN` 空着 → SDK 静默跳过 init
2. release 找不到 → 看是不是 deploy.sh 跑的(本地 `docker build` 直接跑不带 `SENTRY_RELEASE`)
3. sourcemap 没上传 → `SENTRY_AUTH_TOKEN` 没在 `source .env.production` 后导出
4. 浏览器 CSP 拦截 → 检查 `Content-Security-Policy` header(目前没设,但 Phase 2 加时小心)

---

## 6. SSH + Docker 速查

```bash
# 跳板
ssh root@47.238.29.251

# 看前端
docker ps --filter name=cooking-frontend
docker logs -f --tail 100 cooking-frontend
docker inspect cooking-frontend --format '{{json .State.Health}}' | jq

# 进容器(non-root,只装了 wget / sh,没 vim / curl)
docker exec -it cooking-frontend sh

# 看 BFF 是否能解析后端
docker exec cooking-frontend wget -qO- http://cooking-app1-prod:8080/health

# 看网络拓扑
docker network inspect cooking-platform_cooking-prod-net

# 看资源占用
docker stats --no-stream cooking-frontend

# 重启(不换镜像)
cd /opt/cooking-frontend && docker compose -f docker-compose.frontend.yml restart

# 强制重建容器(同镜像)
cd /opt/cooking-frontend && docker compose -f docker-compose.frontend.yml up -d --force-recreate
```

---

## 7. 回滚

镜像在 ECS 上有 `cooking-frontend:<sha>` 历史 tag。回滚 = 改 `TAG` env 起旧版。

```bash
# 看历史镜像
ssh root@47.238.29.251 'docker images cooking-frontend'

# 起旧版(示例回到 SHA db8114a)
ssh root@47.238.29.251 'cd /opt/cooking-frontend && TAG=db8114a docker compose -f docker-compose.frontend.yml up -d --pull never'

# 验证
ssh root@47.238.29.251 'docker exec cooking-frontend wget -qO- http://127.0.0.1:3000/api/health'
```

镜像被 ECS 自动清理(docker prune)的话只能本地 `git checkout <sha>` + 重跑 `./deploy.sh`。建议保留最近 5 个版本镜像。

---

## 8. 已知坑(踩过的,别再踩)

| 坑 | 现象 | 原因 | 解 |
|---|---|---|---|
| `localhost` vs `127.0.0.1` | 容器永远 `(health: starting)` | busybox wget 解析 localhost 偏 IPv6 `[::1]`,Next standalone 只绑 IPv4 `0.0.0.0` | HEALTHCHECK URL 永远写 `127.0.0.1` |
| nginx.conf bind mount | nginx 改了 `nginx.conf` reload 无效 | 单文件 bind mount,inode 没变,nginx 拿旧 fd | 必须 `docker compose up -d --force-recreate nginx` |
| 后端 `--build` 必须 | 后端改 yaml 后 force-recreate 不生效 | Dockerfile `COPY configs/`,配置烤进镜像 | 后端 `up -d --build --force-recreate app1 app2` |
| `mellowck.com/health` 404 | 老监控探针挂 | 阶段 2 后,根域是 Next.js,无 `/health` | 探针切到 `api.mellowck.com/health` |
| `set -a; source .env.production` | Sentry 凭证没生效 | source 没自动 export 到 build 子进程 | `set -a && source .env.production && set +a` |

---

## 9. 凭证 / 联系

| 物 | 在哪 |
|---|---|
| SSH 私钥 | 本地 `~/.ssh/`,已配 known_hosts |
| `.env.production` | 本地 `/Users/xiaoli/Desktop/cooking-frontend/.env.production`(chmod 600),不入 git |
| Sentry 凭证(AUTH_TOKEN 等) | `.env.production` 里;过期重去 [sentry.io](https://sentry.io) Settings → Developer Settings → User Auth Tokens |
| 阿里云 ECS 控制台 | aliyun.com,实例 `47.238.29.251`(HK) |
| 阿里云 OSS bucket | `cooking-platform-prod`(HK),CORS 白名单已加 mellowck.com |
| DNS | 阿里云 DNS 控制台,mellowck.com 三条 A 记录(@/www/api) |
| 域名注册 | 阿里云注册商 |

---

## 10. 何时回 PLAN_DEPLOY.md vs 本文

| 场景 | 看哪 |
|---|---|
| 半夜出事排障 | 本文 §5 |
| 改 / 升级现有 prod | 本文 §4 |
| 新环境从零搭一遍 | `docs/plans/PLAN_DEPLOY.md`(长,含设计 trade-off) |
| 想知道某决策为什么这样 | `PLAN_DEPLOY.md` + `docs/rules/ADR/` |
| 后端阶段 2 落地的实测证据 | `docs/handoff/backend-deploy-output.md` |
