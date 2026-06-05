# 后端改造 punch list · 前端 Phase 1 启动前

> **受众**:后端项目 `/opt/cooking-platform/` 的维护者(包括 Claude Code)。
> **来源**:前端项目 `cooking-frontend` 在 `phase-0-done`(2026-05-27)后的衔接文档。
> **目的**:列出前端 Phase 1 业务开发开始前,后端需要做的所有改动与验证项。
>
> **dev vs prod 适用区分**:
> - 标 **🔴 必须**:前端真上线(prod)前必须完工,**否则 nginx 流量切前端后炸**。
> - 标 **🟡 建议**:不阻塞上线,但建议同期完成。
> - 标 **🔵 验证**:不需要改动,但需后端 owner 现场确认一遍。
>
> **dev 不阻塞**:本清单 0 项阻塞 Phase 1 dev——前端 dev 阶段 `BACKEND_URL=https://mellowck.com`,经现有公网 nginx(目前还允许 `/api/v1/*` 直转)→ 后端,链路通。所有改动只在**真上线日**集中落地。

---

## 0. 背景速读

前端项目接入方式:**BFF(Backend-for-Frontend)全量代理**。

- 浏览器**只**访问 `https://mellowck.com/api/<bff-path>`(BFF 由 Next.js 容器服务,端口 3000)
- BFF Route Handler 拿浏览器请求 → 加 `Authorization: Bearer <access>` 头(从 HttpOnly cookie 读)→ 转发到内部 `app1:8080`(docker 网络 `cooking-platform_cooking-prod-net` 内)
- token / refresh / 401002 静默刷新 全在 BFF Node runtime 完成,**浏览器永远见不到 token**

**关键路径变化**(改造前后对照):

```
改造前(目前线上):
  Browser ──> nginx:443 ──> app1:8080
              (/api/v1/* 直转后端)

改造后(Phase 1 上线):
  Browser ──> nginx:443 ──> frontend:3000 ──> app1:8080
              (catch-all,           (BFF Route Handler,
               包括 /api/*)           Authorization 头在此注入)
```

完整架构参考前端仓库 `docs/rules/ADR/0001-frontend-deployment-bff.md`。本文档下面所有改动都是为实现这条新路径。

---

## 1. 🔴 必须改:nginx 配置(改造主体)

**改主体**:`/opt/cooking-platform/deploy/nginx/nginx.conf`(后端项目内)。
**改动范围**:**只动** `server { listen 443 }` 块内的 `location`;`upstream cooking_backend`、`limit_req_zone`、SSL/HSTS、HTTP→HTTPS 跳转、`http {}` 块顶层指令**全部保留**。

### 1.1 location 块改动对照表

| 原 location | 改动 | 改后形态 / 备注 |
|---|---|---|
| `location ^~ /.well-known/acme-challenge/ { ... }` | **保留不动** | certbot 续期路径 |
| `location = /metrics { return 404; }` | **保留不动** | 防 metrics 泄漏 |
| `location ^~ /health { proxy_pass http://cooking_backend; ... }` | **保留不动** | 运维直查 Go 健康,不经前端;前端有自己的 `/api/health`,各管一段 |
| `location = /api/v1/auth/send-code { proxy_pass http://cooking_backend; limit_req zone=sms_zone burst=N nodelay; ... }` | **改路径 + 改 upstream** | 新形态:`location = /api/auth/send-code { proxy_pass http://frontend:3000; limit_req zone=sms_zone burst=N nodelay; }`(sms_zone 保持) |
| `location /api/ { proxy_pass http://cooking_backend; limit_req zone=api_zone burst=20 nodelay; ... }` | **整段删除** | 让 catch-all 接管,所有 `/api/*` 都走前端 BFF |
| `location / { proxy_pass http://cooking_backend; limit_req zone=api_zone burst=20 nodelay; ... }` | **改 upstream + 补 limit_conn** | `proxy_pass http://frontend:3000;`,其它(api_zone、proxy_set_header、timeout)**全保留**。**新增** `limit_conn conn_zone 20;`——见 §1.6,原 `/api/` 块若有 `limit_conn` 不能丢 |

### 1.2 改后 location 块顺序(自上而下)

```
1. location ^~ /.well-known/acme-challenge/    # certbot
2. location = /metrics                          # 404
3. location ^~ /health                          # 后端直查
4. location = /api/auth/send-code               # 改路径 + frontend upstream + sms_zone
5. location /                                   # catch-all → frontend
```

### 1.3 关键不变项(改造时**禁动**)

- `upstream cooking_backend { server app1:8080; server app2:8080; keepalive 32; }` 块——`/health` 还在用它
- `limit_req_zone $binary_remote_addr zone=sms_zone:10m rate=1r/m;` 等所有 zone 定义
- `proxy_next_upstream` / `client_max_body_size` / `proxy_read_timeout` 等 proxy 公共配置
- `add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;`
- HTTP→HTTPS 重定向 server 块

### 1.4 为什么 sms_zone 套在 `/api/auth/send-code`(前端路径)上

`sms_zone` 是按 client IP 做的滑动窗口限流——nginx 在收到浏览器请求时即套用,**与后端是谁无关**。前端 BFF 收到这个请求后再调后端 `POST /api/v1/auth/send-code`,但前端→后端是 docker 内网,IP 是 nginx 容器内 IP,不再受 sms_zone 限制——所以**只能在公网入口套**,即前端路径。

### 1.5 为什么前端只暴露 `/api/auth/send-code`(不带 v1)

前端 BFF Route Handler 路径**约定**用 `/api/<endpoint>`(无版本号),前端内部转发时再带上 `/api/v1/`。这样后端 API 版本演进时,前端 BFF 是适配层——不污染浏览器侧 URL。

### 1.6 catch-all `location /` 必须补 `limit_conn`

**背景**:原 `location /api/` 块若挂了 `limit_conn conn_zone N`(每 IP 并发连接数限制),整段删除后,catch-all `location /` 现仅继承 `limit_req`(每秒速率)——**并发连接限制悄悄消失**。前端 BFF 是同步代理(每个浏览器请求阻塞一条上游连接),没有 `limit_conn` 时单 IP 可拖死 nginx worker 连接池。

**改动**:catch-all 块**新增** `limit_conn conn_zone 20;`(沿用原 `/api/` 块的 zone 名与阈值,如后端实际值不是 20 → 沿用实际值)。

**前置**:`http {}` 块顶层须有 `limit_conn_zone $binary_remote_addr zone=conn_zone:10m;`——如已存在则**保留不动**;如未定义(原 `/api/` 块没用 limit_conn 也是合理的)→ 跳过本节,catch-all 不补 limit_conn。

**owner 确认点**:后端 owner 在 nginx.conf 内 `grep limit_conn` → 决定是否落地本节。

---

## 2. 🔴 必须改:nginx 容器 docker DNS 解析

**目标**:nginx 容器内能解析 `frontend`(前端容器名)。

**前置**:前端容器 `cooking-frontend-prod` 已加入 docker 网络 `cooking-platform_cooking-prod-net`。
- 前端 compose 补丁文件:前端仓库 `docs/deploy/frontend-service.yml`(用 `external: true + name` 引用已存在的网络,**不重定义**)
- nginx 容器已在该网络内(后端 compose 原本就是)

**改动**:**无**——只要前端容器跑起来,docker DNS 自动可达。验证:

```bash
ssh root@47.238.29.251
docker exec cooking-nginx-prod nslookup frontend
# 期望:Address: <某个 172.x.x.x 容器 IP>
```

**坑**:`nginx -s reload` **不重读** upstream DNS(nginx alpine 镜像默认无 ngx_resolver 动态解析)。前端容器 IP 变化时(重启 / 更新),必须 `docker compose restart nginx`,**不用 reload**。本文档相关 deploy 流程已写在 `cooking-frontend/docs/deployment.md` §3.3。

---

## 3. ⚠️ 不能改:docker-compose nginx `depends_on` 跨 compose 无效

**结论**:**不要**给 nginx 加 `depends_on: frontend`——前端容器在**独立 compose 项目**(前端仓库 `docs/deploy/frontend-service.yml`),`depends_on` 不能跨 compose project 引用 service 名,**强加会让后端 compose 启不来**(`service "frontend" is undefined`)。

**为什么前端单独 compose**:前端仓库 `cooking-frontend` 与后端仓库 `cooking-platform` 是独立 git repo / 独立 owner / 独立 CI;两者只通过 docker external network(`cooking-platform_cooking-prod-net`)互通。这是 ADR-0001 的拍板架构,**不要为了 depends_on 把两个项目并入一个 compose**——会破坏仓库边界。

**冷启动 502 怎么办**:依赖**启动顺序约束**(本文档 §7.1 步骤 5-6):前端容器 owner 先 `docker compose up -d` 起前端、待 healthy → 后端 owner 才 `docker compose restart nginx`。两个 owner 协调一次,避免引入跨 compose 耦合。

**若实在想自动化**:写一段 systemd unit 或 cron 脚本,在物理机重启时按"frontend healthy → nginx restart"顺序兜底。这超出本文档范围,运维侧决定。

**MVP 默认**:接受冷启动期间几秒 502;reboot 频率极低(prod 物理机长期不重启),不值得为此打破仓库边界。

---

## 4. 🟡 建议改:CORS allowed_origins 收敛

**文件**:`/opt/cooking-platform/configs/config.prod.yaml`(L159 附近,`cors:` 块)。

**现状**(`99-prd-deltas H2` 记录):

```yaml
cors:
  allowed_origins:
    - "https://cooking.example.com"   # 占位,go-live 前替换
```

**改为**:

```yaml
cors:
  allowed_origins:
    - "https://mellowck.com"
```

### 4.1 为什么 CORS 实际不阻塞前端?

前端用 BFF 设计,所有请求都从浏览器走**同源** `https://mellowck.com/api/*` → BFF(Node 服务端)→ 后端。BFF→后端是 **server-side fetch**,**不触发 CORS**(CORS 是浏览器机制)。所以即便后端 CORS 是错的占位值,前端业务也不受影响。

但**严格起见**:
- 若有运维 / 第三方工具要从浏览器直接 hit `/api/v1/*`(绕 BFF,排障用),CORS 不通会被浏览器拦
- `release` 模式下 `allowed_origins: ["*"]` 会被 `config.Validate` 拒启动(后端项目 `CLAUDE.md §4 红线`)——目前是 `cooking.example.com` 占位虽然不是 `*`,但不正确

→ 收敛到 `https://mellowck.com` 是**合规级**改动,Phase 1 上线同步落地。

---

## 5. 🟡 建议改:audit.provider 切真实 Aliyun

**文件**:`/opt/cooking-platform/configs/config.prod.yaml`(`audit:` 块)。

**现状**(`99-prd-deltas H1` 记录):

```yaml
audit:
  provider: mock     # 注释写 "Production uses real Aliyun SMS",但实际仍是 mock
```

**改为**(go-live 真要内容审核时):

```yaml
audit:
  provider: aliyun
  aliyun:
    access_key_id: ${ALIYUN_GREEN_AK}        # 从 .env.prod 注入
    access_key_secret: ${ALIYUN_GREEN_SK}
    region: cn-shanghai                       # 或配置中既定值
```

**影响前端的点**:
- mock provider 在 dev 环境下 0-300ms 内将 `audit_status` 0→1(通过)
- aliyun provider 在 prod 5-10s 内通过(`99-prd-deltas B1`)
- 前端 Phase 1 任务 **T-1-D5 审核轮询**已按 30s 上限设计,**两种 provider 都兼容**——不需要前端配合改动

**不阻塞 Phase 1 dev**(dev 用 mock 就够);**真上线**前必须切。

---

## 6. 🔵 验证(不需改,需 owner 现场确认)

### 6.1 全部 Phase 1 endpoints 在线

按前端 `docs/api-spec/00-overview.md §3.5` 鉴权矩阵验证每条都返预期。

**⚠️ 不要在 host shell 用 `http://app1:8080`**——host 没有 docker DNS,`app1` 不可解析。两种正确写法,任选其一:

**⚠️ app1 容器无 curl / wget**(Go 镜像精简,不带 shell 工具)——以下两种写法都用**外部容器**打 docker 内网,owner 任选其一。

**方法 B:用 nginx 容器内置 wget 打**(最轻,无新镜像):

```bash
ssh root@47.238.29.251
WGET='docker exec cooking-nginx-prod wget -qO-'   # nginx alpine 自带 wget

# 公开接口(应返 200 或可预期的业务错)
$WGET http://app1:8080/api/v1/feed?size=5           | jq '.code'   # 期望 0
$WGET http://app1:8080/api/v1/posts/1               | jq '.code'   # 期望 0 或 412104
$WGET "http://app1:8080/api/v1/search?q=test"       | jq '.code'   # 期望 0
$WGET http://app1:8080/api/v1/users/1               | jq '.code'   # 期望 0 或 412105

# 鉴权接口(无 token 应返 401001)
$WGET http://app1:8080/api/v1/users/me              | jq '.code'   # 期望 401001
# POST 用 wget 麻烦,POST 走方法 C
```

**方法 C:起一次性 `curlimages/curl` 容器**(最灵活,GET/POST/-I 全套支持):

```bash
ssh root@47.238.29.251
NET=cooking-platform_cooking-prod-net   # docker network 实名,见前端 ADR-0001
CURL="docker run --rm --network $NET curlimages/curl:latest -sS"

$CURL http://app1:8080/api/v1/feed?size=5           | jq '.code'   # 期望 0
$CURL http://app1:8080/api/v1/users/me              | jq '.code'   # 期望 401001
$CURL -X POST http://app1:8080/api/v1/posts         | jq '.code'   # 期望 401001
$CURL -X POST http://app1:8080/api/v1/upload/presign| jq '.code'   # 期望 401001
# 其余同方法 B 路径
```

> 方法 C 首次拉镜像约 5MB,后续验证用缓存。CI 也可直接复用。

**方法 D:从公网走 nginx**(端到端,但**只**在 nginx 改造**前**有效——改造后 `/api/v1/*` 会 404,因为 nginx 不再有此 location):

```bash
BASE=https://mellowck.com
curl -sS $BASE/api/v1/feed?size=5 | jq '.code'
# 其余同上
```

**改造完工后**验证用方法 B 或 C(后端直链)+ 前端业务路径(`curl https://mellowck.com/api/feed?size=5`,经 BFF 转发)。

### 6.2 OSS bucket CORS 允许浏览器 PUT

前端 T-1-D 组(发帖 + 上传)需要浏览器**直接 PUT 到 OSS**(`91-oss-upload.md` 三步流程的第 2 步):

```
Browser ── PUT ──> https://<bucket>.oss-cn-<region>.aliyuncs.com/<key>?<presign params>
          Headers: Content-Type: image/jpeg
                   (无 Authorization——OSS 用 query 签名)
```

OSS bucket 需要允许:
- **Origin**: `https://mellowck.com`(可加 `http://localhost:3000` / `http://localhost:3100` 给本地 dev)
- **Method**: `PUT, GET, HEAD`
- **Header**: `Content-Type`(必须;其它如 `x-oss-*` 由签名决定,不在浏览器请求侧)
- **Expose-Header**: `ETag`(可选,前端不依赖)

**验证方法**(后端 owner 在阿里云控制台):
```
对象存储 OSS → bucket → 权限管理 → 跨域设置(CORS)
```

如果没配 / 配的不对,前端发帖会在 PUT 阶段 403 `SignatureDoesNotMatch` 或 CORS 错——前端开发期间联调时即可暴露。

### 6.3 refresh 流程后端侧行为

前端 BFF 在 401002(access expired)时**自动**调 `POST /api/v1/auth/refresh`,后端返回**新 access + 新 refresh**(token 对**轮换**)。
验证(POST + JSON body,用 §6.1 方法 C 一次性 curl 容器):

```bash
ssh root@47.238.29.251
NET=cooking-platform_cooking-prod-net
REFRESH='<旧 refresh>'   # 先 login 拿 refresh,假设有测试用户 phone=13900000000

docker run --rm --network $NET curlimages/curl:latest -sS \
  -X POST http://app1:8080/api/v1/auth/refresh \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}" | jq .

# 期望:返 200,data 含 access_token / refresh_token / access_token_expires_at
```

### 6.4 cookie 安全属性后端**不**设置

**确认**:后端 `POST /api/v1/auth/login` 响应**只**在 JSON body 返 `access_token` / `refresh_token`,**不**通过 `Set-Cookie` 头下发。

理由:cookie 由前端 BFF Route Handler 设置(HttpOnly + Secure + SameSite=Lax),后端不参与 cookie 生命周期。如果后端也下发 cookie,会冲突。

验证(POST + 只看 header,用 §6.1 方法 C):

```bash
ssh root@47.238.29.251
NET=cooking-platform_cooking-prod-net

docker run --rm --network $NET curlimages/curl:latest -sSI \
  -X POST http://app1:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13900000000","code":"000000"}' | grep -i '^set-cookie'

# 期望:无输出(后端不下发 cookie)
```

后端已 grep 确认 `user_service.go` 无 `Set-Cookie` 逻辑(2026-05-27 backend Claude 验证);本节作为**回归点**保留——任何后续后端鉴权改动后跑一遍即可。

---

## 7. 改造执行顺序与回滚

### 7.1 推荐执行顺序

1. **§4 CORS 收敛**:改 `config.prod.yaml`
2. **§5 audit.provider**:改 `config.prod.yaml`(可与 §4 同 commit)
3. **§6 验证**:逐项 curl(用 §6.1 方法 B / C / D 任选),确认现状 baseline
4. **§1 nginx 改造**:改 `nginx.conf`(含 §1.6 catch-all limit_conn,若适用)
5. **前端容器先起**:前端项目 owner 用前端仓库 `docs/deployment.md §3` 流程把 `cooking-frontend-prod` 起来,**healthy 后**才继续(此步替代 §3 被拒掉的 `depends_on`,由 owner 手动协调顺序)
6. **重启 nginx**:`docker compose -f docker-compose.prod.yml --env-file .env.prod restart nginx`(用 `restart` 不用 `reload`,见 §2 脚注)
7. **§6 验证再跑**:确认改造后行为一致(`/api/v1/users/me` 现在应该 404,因为 nginx 不再有 `/api/v1/*` location;但 `/api/users/me` 走前端 BFF 转后端后又能通)

### 7.2 回滚

任一步出错:

- nginx 改造回滚:`git -C /opt/cooking-platform checkout deploy/nginx/nginx.conf && docker compose ... restart nginx`
- 前端容器无害,不需停(只是公网流量不再经它)
- config 改动 git 回滚 + `docker compose ... up -d` 让对应 service pick up
- 数据无损——本次改造**纯路由/配置层**,无数据库 migration

---

## 8. 改造完工后需告知前端的事

完工后请通知前端项目 owner 以下信息,前端项目相应更新 `docs/sessions/` 与 smoke checklist:

| 项 | 期望内容 |
|---|---|
| nginx 改造 git commit hash | 后端项目内 `git log -1 deploy/nginx/nginx.conf` |
| config 改动 commit hash(§4 CORS / §5 audit) | `git log -1 configs/config.prod.yaml` |
| OSS CORS 配置已确认通过 | 截图或控制台 URL |
| audit.provider 是否切 aliyun | yes/no + 切换时间 |
| 真上线日期 | 用以前端打 `phase-1-live` tag |

---

## 9. 不在本文档范围

- **后端业务代码改动**:Phase 1 范围内,前端发现任何 endpoint 行为与 `docs/api-spec/` 不符 → 起 ADR + 跨仓库讨论;**不**预设后端 endpoint 需改造,目前 `docs/api-spec/` 全部以真实代码为准
- **数据库 migration**:本次纯配置 / 路由层
- **新 endpoint**:Phase 1 用到的所有 endpoint 已存在(`00-overview.md §3.5`),无需新加
- **CI/CD**:前后端项目各自的 CI 流程,本文档不动
- **多实例 HA**:`ADR-0001 §7` 列为待办,不属于 Phase 1 上线前

---

## 10. 联系

前端项目 owner:本仓库主用户。
前端 Claude Code 工作目录:`/Users/xiaoli/Desktop/cooking-frontend`。
任何疑问以前端仓库 `docs/rules/ADR/0001-frontend-deployment-bff.md` + `docs/api-spec/` 为准。

本文档生成时间:2026-05-27,基于 `phase-0-done` tag(commit `d73a4d5`)。
