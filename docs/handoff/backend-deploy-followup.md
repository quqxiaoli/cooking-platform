# 后端 follow-up · 前端上线部署衔接

> **受众**:后端项目 `/opt/cooking-platform/` 的维护者(包括 Claude Code)。
> **来源**:前端项目 `cooking-frontend` 准备上线,前端拿主域名 `mellowck.com`,后端迁子域 `api.mellowck.com`。
> **目的**:列出后端需做的 nginx / TLS / CORS / 文档改动,前端等这一组改动完成才能完整上线。

姊妹文档:
- `docs/handoff/backend-changes-pre-phase-1.md`(Phase 1 启动前的改造清单,已完工)
- `docs/handoff/backend-followup-post-phase-1.md`(Phase 1 完工后的 `is_following` / `liked_by_me` 字段补丁,**与本文档无依赖**,可独立排期)

完整上下文见前端项目 `docs/plans/PLAN_DEPLOY.md`。本文档是 §3.4 + §4.3 的镜像。

---

## 0. 概览

| 项 | 标签 | 阻塞前端? |
|---|---|---|
| 1. nginx server_name 拆分:`api.mellowck.com` → 后端,`mellowck.com` → 前端 + /api/v1 兜底 301 | 🔴 必做 | 是 |
| 2. TLS 证书签发(`api.mellowck.com`) | 🔴 必做 | 是 |
| 3. 后端 CORS allowed origin 加 `https://mellowck.com` | 🔴 必做 | 是(发帖 OSS 链不直接受影响,但 future-proof) |
| 4. `docs/api-spec/00-overview.md` base URL 改写 | 🟡 建议 | 否(文档同步,不阻塞接口) |

**共 4 项**。1-3 是 ship 前必做且原子(同一窗口里跑完 + nginx `--force-recreate`,**不是** reload——见 §2.1 inode 说明),4 是文档跟进。

---

## 1. 前置(前端 / 用户已完成)

| 项 | 状态 | 说明 |
|---|---|---|
| DNS A 记录 `api.mellowck.com` → `47.238.29.251` | 等用户在阿里云控制台加 | `dig +short api.mellowck.com` 返 IP 即生效;**TLS 签发前必须先生效**(certbot HTTP-01 challenge 走的就是这条 DNS) |
| OSS bucket CORS allowed origin `https://mellowck.com` | 等用户在阿里云 OSS 控制台加 | 与后端 nginx 无关,但前端发帖上传(浏览器 PUT 到 OSS)需要;此项漏掉 → smoke Step 2 发帖闭环挂 |
| `www.mellowck.com` 归属决策 | 等用户确认 | 现 nginx 同时响应 `mellowck.com` + `www.mellowck.com`。拆分后两种选择:(A)前端也接管 www.,server B 加 `server_name`;(B)www. 直接 301 到 apex。**本文档按 (A) 写**——前端接管,SSL 证书签 `mellowck.com` 时一并把 `www.mellowck.com` SAN 进去(现状如此则无需重签) |

后端这边等 DNS 生效后开干。

---

## 2. 改动详情

### 2.1 nginx server 块拆分

**位置**:`/opt/cooking-platform/deploy/nginx/nginx.conf`(单文件 bind mount 到 `cooking-nginx-prod:/etc/nginx/nginx.conf:ro`,见 `docker-compose.prod.yml`)。

**当前态**(已 verify 自仓库):
- `http` 块全局 `client_max_body_size 6m`、`limit_req_zone api_zone:30r/s`、`limit_req_zone sms_zone:1r/m`、`upstream cooking_backend { app1:8080; app2:8080; keepalive 32; }`
- :80 server `server_name mellowck.com www.mellowck.com;` + `/.well-known/acme-challenge/` webroot + 其余 301 https
- :443 server `server_name mellowck.com www.mellowck.com;`,location 包含:`= /metrics → 404`(红线 §10.3)、`^~ /health`、`= /api/v1/auth/send-code`(sms_zone)、`/api/`(api_zone burst=50)、`/`(api_zone burst=20)
- 浏览器访问 `https://mellowck.com/` 落到后端 Go 服务,Go 没 `/` 路由 → 404(用户观测一致)

**目标态**:两个 :443 server 块共存,**:80 server 一个就够**(server_name 加 api.)。

⚠️ **改前必做**:
```bash
# 1. 先备份(回滚靠它)
cp /opt/cooking-platform/deploy/nginx/nginx.conf \
   /opt/cooking-platform/deploy/nginx/nginx.conf.bak
```

⚠️ **配置文件改完后**:**不要** `nginx -s reload`。本仓库 `nginx.conf` 是单文件 bind mount,宿主机编辑后 inode 变了,容器仍持旧 inode,reload 无效(CLAUDE.md §9.2)。必须用 `--force-recreate`:
```bash
cd /opt/cooking-platform
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --force-recreate nginx
```

```nginx
# ── :80 全 301 https + ACME challenge 路径 ────────────────────────────
# 关键:server_name 必须包含 api.mellowck.com,否则 certbot webroot 模式签
# api. 证书时,HTTP-01 challenge 走 default server,/.well-known/acme-challenge/
# 不可达,签发失败。
server {
    listen 80;
    listen [::]:80;
    server_name mellowck.com www.mellowck.com api.mellowck.com;

    location ^~ /.well-known/acme-challenge/ {
        root /var/www/certbot;
        default_type "text/plain";
        try_files $uri =404;
    }

    location / {
        return 301 https://$host$request_uri;
    }
}

# ── server A: api.mellowck.com → 后端 ─────────────────────────────────
# 现 mellowck.com :443 的 location 集合整体迁过来,api. 是后端的唯一入口。
server {
    listen 443 ssl;
    http2 on;
    server_name api.mellowck.com;

    ssl_certificate     /etc/letsencrypt/live/api.mellowck.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/api.mellowck.com/privkey.pem;

    # TLS / HSTS — 直接复制现有 mellowck.com server 的 ssl_protocols / ssl_ciphers /
    # ssl_session_*  / Strict-Transport-Security 几行,完全一致
    # (此处省略,改文件时照搬现有 4 行 ssl_* + 1 行 add_header HSTS)

    proxy_next_upstream         error timeout http_503;
    proxy_next_upstream_tries   2;
    proxy_next_upstream_timeout 10s;

    # 红线 §10.3:API 子域更不能暴露 /metrics
    location = /metrics {
        return 404;
    }

    # /health 不走限流(Docker healthcheck / Prometheus probe)
    location ^~ /health {
        proxy_pass         http://cooking_backend;
        proxy_http_version 1.1;
        proxy_set_header   Connection        "";
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_connect_timeout 5s;
        proxy_read_timeout    10s;
        proxy_send_timeout    10s;
    }

    # SMS send-code — 严格 per-IP brake,跟随 API 迁到 api.*
    location = /api/v1/auth/send-code {
        limit_req zone=sms_zone burst=2 nodelay;
        limit_conn conn_zone 5;
        proxy_pass         http://cooking_backend;
        proxy_http_version 1.1;
        proxy_set_header   Connection        "";
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_connect_timeout 5s;
        proxy_read_timeout    30s;
        proxy_send_timeout    30s;
    }

    # 默认接所有 API
    location / {
        limit_req  zone=api_zone burst=50 nodelay;
        limit_conn conn_zone 20;
        proxy_pass         http://cooking_backend;  # ← 仓库实际 upstream 名
        proxy_http_version 1.1;
        proxy_set_header   Connection        "";
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_connect_timeout 5s;
        proxy_read_timeout    60s;
        proxy_send_timeout    60s;
    }
}

# ── server B: mellowck.com / www.mellowck.com → 前端(+ /api/v1 301 兜底)─
server {
    listen 443 ssl;
    http2 on;
    server_name mellowck.com www.mellowck.com;

    ssl_certificate     /etc/letsencrypt/live/mellowck.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/mellowck.com/privkey.pem;

    # TLS / HSTS — 同 server A,直接复制现有 mellowck.com 配置

    # 兜底:野生 curl / 老外链 https://mellowck.com/api/v1/* → 301 到 api.
    # 用 ^~ 让前缀匹配优先,绕开下面更宽的 /api/。两个 ^~ 同时存在时按最长前缀取,
    # 写在前面只是为了可读性。
    location ^~ /api/v1/ {
        return 301 https://api.mellowck.com$request_uri;
    }

    # 前端 BFF Route Handler(/api/auth/* / /api/users/me / /api/posts/* 等,不带 v1)
    location ^~ /api/ {
        proxy_pass http://cooking-frontend:3000;
        proxy_http_version 1.1;
        proxy_set_header Connection        "";
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # 页面 / 静态资源 → Next.js
    location / {
        proxy_pass http://cooking-frontend:3000;
        proxy_http_version 1.1;
        proxy_set_header Connection        "";
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        # /_next/static/* 由 Next 自带 cache-control: immutable,nginx 无需再加
    }
}
```

**关键点**:
- **`client_max_body_size 6m` 在 http 块全局已声明,server A/B 都继承,不需要重写**
- `cooking-frontend` 是前端容器 service 名,需与后端 docker network 同网(见第 7 节命令 4 验证)
- HSTS / TLS 协议 / cipher / session 等沿用现有 `mellowck.com` 安全配置——直接复制对应 5-6 行,不要重写

**验证**(注意是 `--force-recreate` 不是 reload):
```bash
# 1. 容器内语法检查(此时配置还是旧的,nginx -t 校验的是宿主机文件内容?不,
#    必须在 force-recreate 之前用 docker run 一次性 dry run,或直接 force-recreate
#    后看 nginx 是否启动成功)
# 推荐做法:用 nginx 镜像在 host 上做语法预检
docker run --rm \
  -v /opt/cooking-platform/deploy/nginx/nginx.conf:/etc/nginx/nginx.conf:ro \
  -v /etc/letsencrypt:/etc/letsencrypt:ro \
  nginx:1.27-alpine nginx -t

# 2. force-recreate(reload 无效,见 CLAUDE.md §9.2)
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --force-recreate nginx

# 3. 确认容器内文件 sha 与宿主机一致(防 inode 坑回归)
sha256sum /opt/cooking-platform/deploy/nginx/nginx.conf
docker exec cooking-nginx-prod sha256sum /etc/nginx/nginx.conf

# 4. api.* 通
curl -s -i https://api.mellowck.com/health
# 期望:HTTP/2 200

# 5. 兜底 301 通
curl -s -i https://mellowck.com/api/v1/feed?size=1
# 期望:HTTP/2 301 + Location: https://api.mellowck.com/api/v1/feed?size=1

# 6. mellowck.com / 前端容器没起前预期 502;前端起后再验 200
curl -s -i https://mellowck.com/
```

---

### 2.2 TLS 证书(`api.mellowck.com`)

**前置(双重)**:
1. DNS `api.mellowck.com` 已生效(`dig +short api.mellowck.com` 返 `47.238.29.251`)
2. **nginx :80 server_name 已包含 `api.mellowck.com` 且容器已 force-recreate**(见 §2.1 :80 block)。否则 ACME challenge HTTP 请求落到 default server,webroot 找不到 challenge 文件,签发失败

**操作**(沿用现有 certbot 流程):

```bash
docker run --rm \
  -v /etc/letsencrypt:/etc/letsencrypt \
  -v /opt/cooking-platform/deploy/certbot/webroot:/var/www/certbot \
  certbot/certbot certonly --webroot -w /var/www/certbot \
  -d api.mellowck.com \
  --non-interactive --agree-tos -m <运维邮箱>
```

**验证**:
```bash
ls /etc/letsencrypt/live/api.mellowck.com/
# 期望:fullchain.pem  privkey.pem  cert.pem  chain.pem
```

**续签 hook(重要)**:
- `certbot renew` 默认会处理所有已签证书,但**不会**自动通知 nginx 重载新证书
- 当前 mellowck.com 续签后 nginx 怎么 reload?**改 api. 之前必须先 verify**——如果当前没有 deploy-hook,本次一起补上
- 推荐做法(host certbot 直接装时):
  ```bash
  # /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh
  #!/bin/sh
  # cert 是目录挂载,inode 不变,reload 在这里是有效的
  docker exec cooking-nginx-prod nginx -s reload
  ```
  `chmod +x` 后 certbot renew 会自动调
- 验证:`certbot renew --dry-run` 应看到 hook 触发日志

---

### 2.3 后端 CORS allowed origin

**位置**:`configs/config.prod.yaml` 顶级 key `cors.allowed_origins`(中间件 `middleware.CORS(corsCfg)` 在 `cmd/server/main.go:408` 装配)。

**当前态**(已 verify 自仓库):
```yaml
cors:
  allowed_origins:
    - "https://cooking.example.com"   # ← 占位符,从未替换
```

**改动**:**替换**(不是追加)占位符为真实域名。

```yaml
cors:
  allowed_origins:
    - "https://mellowck.com"
    - "https://www.mellowck.com"     # 如保留 www. 接入
```

**注意**:
- `config.Validate` 在 release mode + `*` 组合时会拒绝启动(红线 CORS-01),所以不能图省事写 `*`
- 三份 yaml 是否需要同步?**只改 prod**——dev/docker 的 CORS 保留 localhost 用于本地联调,见 §4.1 白名单
- 改后:`docker compose ... up -d --force-recreate app1 app2`(配置卷挂载 + 首次 inode 失效兜底,见 CLAUDE.md §7.3)

**理由**:
- 当前 BFF 模式下浏览器**不**直接调 `api.mellowck.com`(都走前端 BFF),所以本项**当下不阻塞**
- 但 future(移动端 app / 第三方调试 / curl with Origin header / Web Vitals beacon 等)可能需要——上线前一次性放好,避免日后出问题再追改

**验证**:
```bash
# 用 OPTIONS 预检更准(GET 不一定回 Access-Control-* 头)
curl -s -i -X OPTIONS https://api.mellowck.com/api/v1/feed \
  -H "Origin: https://mellowck.com" \
  -H "Access-Control-Request-Method: GET"
# 期望响应头含:
#   Access-Control-Allow-Origin: https://mellowck.com
#   Access-Control-Allow-Methods: GET,POST,...
#   Access-Control-Allow-Credentials: true  (如果带 cookie 调)
```

---

### 2.4 文档同步

**位置**:`docs/api-spec/00-overview.md`(后端仓库)

**改动**:
- base URL 描述从 `https://mellowck.com/api/v1` → `https://api.mellowck.com/api/v1`
- 加一句迁移说明:"2026-05-31 起后端迁子域 `api.mellowck.com`,旧 `mellowck.com/api/v1/*` 走 301 兜底"

**验证**:`git log` 见 commit + 文中 URL 全部替换

---

## 3. 执行顺序

⚠️ 顺序硬约束:**先改 :80 server_name(让 ACME challenge 可达)→ 再签证书 → 再补 :443 server A/B**。颠倒了第一步,证书签不下来。

1. **等前置**:用户加 DNS A 记录(`api.mellowck.com` → `47.238.29.251`)
2. **备份 nginx.conf**:`cp deploy/nginx/nginx.conf{,.bak}`
3. **改 :80 server**(§2.1 :80 block):把 `server_name` 改成 `mellowck.com www.mellowck.com api.mellowck.com`,其余暂不动
4. **force-recreate nginx**:`docker compose ... up -d --force-recreate nginx`
5. **签证书**(§2.2,DNS + :80 都已就绪)
6. **改 :443 server A/B 配置**(§2.1 主体):此时 server B 的 `cooking-frontend:3000` upstream 还没起,**先别 force-recreate**——继续往下
7. **改后端 CORS**(§2.3,与 nginx 改无依赖,可同步进行)
8. **前端**:在 handoff 文档 / 共享渠道告知前端"你可以 docker push 了",前端起容器
9. **联调**:前端容器起后,后端 `force-recreate nginx`(见 §2.1 末尾的验证命令)
10. **smoke**:前端跑 Phase 1 smoke,任何挂了立即排查
11. **配 certbot deploy-hook**(§2.2 续签 hook):防 90 天后证书过期没人 reload
12. **文档同步**(§2.4):smoke 通过后改 base URL 文档

---

## 4. 是否必须?

**1 + 2 + 3 必做**——前端用 `mellowck.com` 这个方案的核心约束。否则前端没地方接入。

**4 是建议**——文档不同步不会让接口挂,但 API 消费者(包括 future 移动端 / 第三方)会被旧 URL 误导。半天工作量内顺手做掉。

---

## 5. 是否影响后端业务逻辑?

**不影响**。本次改动只在网关层(nginx)+ CORS 配置 + 文档,Go 服务代码 / DB / Redis / OSS 业务逻辑零修改。

---

## 6. 回滚

前提:§3 步骤 2 已经 `cp nginx.conf{,.bak}` 备份过。

```bash
# 1. 还原配置
cp /opt/cooking-platform/deploy/nginx/nginx.conf.bak \
   /opt/cooking-platform/deploy/nginx/nginx.conf

# 2. force-recreate(reload 不够,见 §2.1 inode 说明)
cd /opt/cooking-platform
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --force-recreate nginx

# 3. 验证容器内文件 sha 回到旧版
sha256sum deploy/nginx/nginx.conf
docker exec cooking-nginx-prod sha256sum /etc/nginx/nginx.conf
```

证书已签的不用回滚——多签一张不影响旧域名。后端 CORS 改动如需回滚:`git checkout configs/config.prod.yaml` + `force-recreate app1 app2`。

---

## 7. 完工后请回传给前端

前端 Claude Code 是**独立 session、独立项目**,跟后端这边没有直接通信通道。回传方式选一个:
- **推荐**:在后端仓库新增 `docs/handoff/backend-deploy-output.md`,把下面命令的实际输出整段贴进去 + `git push`。前端 session 启动时 `read` 此文件即可秒接上下文
- 备选:commit message / Notion / 任何前端能访问的共享渠道

```bash
# 1. /health 通(后端 nginx server A 配好的证据;比 /feed 更可靠,无需 token)
curl -s -i https://api.mellowck.com/health

# 2. /api/v1/* 业务接口通(用 OPTIONS 或带合法 token 的 GET 都行)
curl -s -i https://api.mellowck.com/api/v1/feed?size=1

# 3. 主域 /api/v1/* 走 301(nginx server B 兜底配好的证据)
curl -s -i https://mellowck.com/api/v1/feed?size=1
# 期望:HTTP/2 301 + Location: https://api.mellowck.com/api/v1/feed?size=1

# 4. CORS allowed origin 已生效(用 OPTIONS 预检,GET 不一定回 CORS 头)
curl -s -i -X OPTIONS https://api.mellowck.com/api/v1/feed \
  -H "Origin: https://mellowck.com" \
  -H "Access-Control-Request-Method: GET" \
  | grep -i 'access-control-'

# 5. 后端 docker network 真实名字——前端 docker-compose.frontend.yml 的
#    networks.external.name 字段要对齐(可能是 cooking-platform_default /
#    cooking_default / 别的)
docker network ls --format '{{.Name}}' | grep -i cooking

# 6. /metrics 在 api. 也 404(红线 §10.3 验证)
curl -s -i https://api.mellowck.com/metrics
# 期望:HTTP/2 404
```

第 5 条尤其重要——前端 `docker-compose.frontend.yml` 里 `networks.external.name` 写错的话,前端容器会在独立 network 里跑,nginx 容器解析不到 `cooking-frontend:3000` hostname,起容器后 `https://mellowck.com/` 会 502。

---

## 文档维护

- 本文档完工后:在前端 `DEBT_LEDGER` 新加一条 D-009 / D-010 跟踪"deploy 后端 follow-up 已落",由前端 Claude Code 收尾
- 后续若发现新 deploy 相关 follow-up,在本文档加章节,不开新文档
