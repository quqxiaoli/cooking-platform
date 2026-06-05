# OPS RUNBOOK · 后端运维速查

> **受众**:未来的我(或临时接手的人)。半夜 3 点被叫醒能照着排。
>
> **范围**:后端栈 = Go app(app1/2)+ MySQL 主从 + Redis Sentinel + RabbitMQ + nginx + Prometheus/Grafana/Alertmanager/blackbox + certbot + 阿里云 OSS/SMS/Audit。**不**含前端(让前端 `docs/ops/FRONTEND_OPS_RUNBOOK.md` 兜)。
>
> **关联文档**:
> - `CLAUDE.md`(项目主指南,本文是 §7-9 的浓缩版,适合半夜抢救;CLAUDE.md 适合开工前先读)
> - `docs/project/PROJECT_RETROSPECTIVE.md`(12 个关键决策的背景)
> - `docs/handoff/backend-deploy-output.md`(2026-05-31 阶段 2 上线落地证据)
> - `docs/progress/19_项目进度追踪.md`(Viper 嵌套 env 排查复盘)

---

## 1. TL;DR(域名 / 端口 → 服务一行映射)

| 入口 | 落到 | TLS 到期 |
|---|---|---|
| `https://api.mellowck.com` | nginx server A → `cooking_backend` upstream(app1+app2:8080) | 2026-08-29 |
| `https://api.mellowck.com/health` | app1/2 `/health` 业务探针(无限流) | — |
| `https://api.mellowck.com/metrics` | **故意 404**(红线 §10.3) | — |
| `https://api.mellowck.com/api/v1/auth/send-code` | sms_zone(1r/m + burst=2)严格限流 | — |
| `https://mellowck.com/api/v1/*` | 301 → `https://api.mellowck.com/$1` | — |
| `https://mellowck.com/*` 其余 | nginx server B → `cooking-frontend:3000`(前端域) | 2026-08-29 |
| 内网 `app1:8080/metrics` | Prometheus 直接 scrape(不经 nginx) | — |
| `127.0.0.1:3000`(loopback only) | Grafana,需 SSH 隧道访问 | — |
| 内网 `prometheus:9090` / `alertmanager:9093` | scrape + alert(都不暴露公网) | — |

**注**:`mellowck.com/health` 现在是 **404**(归前端 Next.js,无此路由)。后端的健康探针**必须**打 `api.mellowck.com/health`。

---

## 2. 拓扑

```
公网 443 ──▶ cooking-nginx-prod ──┬─ server A: api.mellowck.com ──▶ cooking_backend upstream
                                  │                                  ├─ cooking-app1-prod:8080
                                  │                                  └─ cooking-app2-prod:8080
                                  └─ server B: mellowck. / www.   ──▶ cooking-frontend:3000(前端)
                                                /api/v1/* → 301 → api.

  app1/2 (Go 1.23 + Gin + GORM + DBResolver)
    │
    ├─ 写库 ──▶ cooking-mysql-prod:3306        (master,GTID)
    ├─ 读库 ──▶ cooking-mysql-slave-prod:3306  (async slave, ≤1s 延迟)
    ├─ 缓存 ──▶ cooking-redis-master-prod:6379 + slave(Sentinel 模式)
    ├─ MQ ──▶ cooking-rabbitmq-prod:5672      (持久 + ACK + DLX)
    │
    ├─ OSS ──▶ 阿里云 cooking-platform-prod(HK,公共读)
    ├─ SMS ──▶ 阿里云短信
    └─ Audit ──▶ 阿里云内容安全 Green API

  监控栈 (内网 only):
    cooking-prometheus-prod (scrape app1/2/mysql_exporter/redis_exporter/blackbox)
        ↕
    cooking-alertmanager-prod (SMTP 发邮件)
        ↕
    cooking-grafana-prod (3000,loopback 绑定,SSH 隧道)
        ↕
    cooking-blackbox-exporter-prod (探针 api.mellowck.com/health)

  docker network: cooking-platform_cooking-prod-net (所有容器同网)
```

**为什么 nginx + 双 Go 实例?** Round-robin LB + 被动健康检查(`proxy_next_upstream error timeout http_503`),一个 app 挂另一个顶。MVP 阶段够,Phase 2 评估横向扩。

**为什么主从分离?** GORM DBResolver Random Policy 把 SELECT 路由到 slave,Step 20 实测 slave 读流量占比 91%。复制延迟 <1s,业务可容忍最终一致;只有"读自己刚写"场景走 master(repository 层主动指定)。

---

## 3. 速查表

### 3.1 容器清单(`docker compose -f docker-compose.prod.yml ps`)

| 容器名 | 镜像 | 端口(host:container) | 用途 |
|---|---|---|---|
| `cooking-app1-prod` | 本地 build | 8080(内网) | Go API 实例 1 |
| `cooking-app2-prod` | 本地 build | 8080(内网) | Go API 实例 2 |
| `cooking-mysql-prod` | mysql:8.0 | 3306(内网) | 主库(GTID, 写) |
| `cooking-mysql-slave-prod` | mysql:8.0 | 3306(内网) | 从库(读) |
| `cooking-redis-master-prod` | redis:7.2-alpine | 6379(内网) | 缓存主 |
| `cooking-redis-slave-prod` | redis:7.2-alpine | 6379(内网) | 缓存从 |
| `cooking-rabbitmq-prod` | rabbitmq:3-mgmt | 5672/15672(内网) | MQ + 管理面 |
| `cooking-nginx-prod` | nginx:1.27-alpine | **80:80 / 443:443** | TLS 终结 + LB |
| `cooking-prometheus-prod` | prom/prometheus | 9090(内网) | Metrics scrape |
| `cooking-alertmanager-prod` | prom/alertmanager | 9093(内网) | 告警 |
| `cooking-blackbox-exporter-prod` | prom/blackbox-exporter | 9115(内网) | 外探针 |
| `cooking-grafana-prod` | grafana/grafana | **127.0.0.1:3000** | 可视化(loopback only) |

**重启某个**:`docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --force-recreate <name>`

### 3.2 关键 env(`/opt/cooking-platform/.env.prod`,权限必须 600)

| 段 | 变量 | 谁读 |
|---|---|---|
| MySQL | `MYSQL_ROOT_PASSWORD` `MYSQL_DATABASE` `REPL_USER` `REPL_PASSWORD` | mysql 容器 + replication |
| Redis | `REDIS_PASSWORD` | redis + app |
| RabbitMQ | `RABBITMQ_USER` `RABBITMQ_PASSWORD` | rabbitmq + app |
| App 加密 | `APP_ENCRYPTION_PHONE_KEY` `APP_ENCRYPTION_PHONE_PEPPER` `APP_JWT_SECRET`(≥32 字符) | app(Viper AutomaticEnv 覆盖 yaml) |
| App DSN | `APP_DATABASE_DSN` `APP_DATABASE_SLAVES_DSN` `APP_MQ_URL` | app |
| 阿里云 | `APP_OSS_ACCESS_KEY_ID/SECRET` `APP_SMS_*` `APP_AUDIT_*` | app |
| 监控 | `APP_GRAFANA_ADMIN_PASSWORD` `ALERT_SMTP_*` `ALERT_EMAIL_TO` | grafana + alertmanager |

> **关于 Viper 嵌套 env**:Viper AutomaticEnv 对**简单 key 字符串** 覆盖 OK(`APP_JWT_SECRET` 覆盖 `jwt.secret`)。但**数组/嵌套**(如 `cors.allowed_origins`)用 env 覆盖会踩坑——见 `docs/progress/19_*.md`。安全方案:数组类配置改 yaml + `--build --force-recreate`(yaml 烤进镜像)。

### 3.3 配置文件位置

| 文件 | 用途 | 改完怎么生效 |
|---|---|---|
| `configs/config.prod.yaml` | 业务配置(CORS/限流/DB pool/log 等) | **必须** `docker compose ... up -d --build --force-recreate app1 app2`(yaml 是 `COPY` 进镜像的,见已知坑 §8) |
| `deploy/nginx/nginx.conf` | nginx 全配 | `--force-recreate nginx`(bind mount 单文件,reload 无效,见已知坑 §8) |
| `deploy/prometheus/prometheus.yml` | scrape 目标 | bind mount,`docker exec cooking-prometheus-prod kill -HUP 1`(SIGHUP 重读) |
| `deploy/alertmanager/alertmanager.yml.tmpl` | 告警路由 | init 容器渲染,`--force-recreate alertmanager-config alertmanager` |
| `deploy/blackbox/blackbox.yml` | 外探针配置 | `--force-recreate blackbox-exporter` |

### 3.4 Make 速查(本地 / 服务器都能跑)

```bash
make build                        # 编译
make test                         # 单测 + race
make lint                         # vet + staticcheck
make check-config-parity          # 三 yaml 顶级 key 一致性
make migrate-up                   # 应用 migration
make migrate-down                 # 回滚最后一个
make migrate-create NAME=xxx      # 新建 migration
make verify-step20                # prod 端到端 + 主从读写分离实证(需 MYSQL_ROOT_PW)
make stress-test                  # wrk2 压测(需 MYSQL_ROOT_PW)
make step-closeout N=20           # 收尾交付物自检
```

---

## 4. 部署 / 维护

### 4.1 改 Go 代码 redeploy

```bash
# 本地
cd ~/Desktop/cooking-platform
make build && make test && make lint
git push origin main

# 服务器
ssh root@47.238.29.251
cd /opt/cooking-platform
git pull origin main
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --build --force-recreate app1 app2

# 验证(等 healthy)
docker ps --filter name=cooking-app --format "{{.Names}}\t{{.Status}}"
curl -s -o /dev/null -w "HTTP %{http_code}\n" https://api.mellowck.com/health
```

**注**:Dockerfile `COPY configs/`,改 yaml 也走这条命令(`--build` 不能省)。

### 4.2 改 nginx.conf redeploy

```bash
# 服务器
cd /opt/cooking-platform
cp deploy/nginx/nginx.conf{,.bak.$(date +%Y%m%d-%H%M%S)}    # 备份带时间戳
git pull origin main

# host 语法预检(必须加 cooking network 解析 upstream)
docker run --rm --network cooking-platform_cooking-prod-net \
  -v /opt/cooking-platform/deploy/nginx/nginx.conf:/etc/nginx/nginx.conf:ro \
  -v /etc/letsencrypt:/etc/letsencrypt:ro \
  nginx:1.27-alpine nginx -t

# force-recreate(reload 无效!)
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --force-recreate nginx

# inode 一致性 verify
sha256sum deploy/nginx/nginx.conf
docker exec cooking-nginx-prod sha256sum /etc/nginx/nginx.conf
```

### 4.3 数据库 migration

```bash
# 本地新建
make migrate-create NAME=add_xxx_field

# 写迁移内容,commit + push

# 服务器
cd /opt/cooking-platform
git pull origin main
make migrate-up

# 验证
RPW=$(grep '^MYSQL_ROOT_PASSWORD=' .env.prod | cut -d= -f2-)
docker exec cooking-mysql-prod mysql -uroot -p"$RPW" -e "SHOW TABLES" cooking
```

破坏性 migration(DROP / ALTER 大表)必须**先发 wave 告警 + 验从库延迟 < 1s** 再执行。

### 4.4 每月巡检(粘贴执行)

```bash
ssh root@47.238.29.251 << 'EOF'
echo "=== 证书 ==="
certbot certificates
echo "=== 磁盘 ==="
df -h && du -sh /var/lib/docker/volumes/* 2>/dev/null | sort -h | tail -10
echo "=== .env.prod 权限(必须 600 root:root)==="
stat -c "%a %U:%G %n" /opt/cooking-platform/.env.prod
echo "=== app 日志体积 ==="
docker exec cooking-app1-prod du -sh /var/log/cooking/
docker exec cooking-app2-prod du -sh /var/log/cooking/
echo "=== 主从复制 ==="
RPW=$(grep '^MYSQL_ROOT_PASSWORD=' /opt/cooking-platform/.env.prod | cut -d= -f2-)
docker exec cooking-mysql-slave-prod mysql -uroot -p"$RPW" -e "SHOW REPLICA STATUS\G" | grep -E 'Seconds_Behind_Source|Replica_IO_Running|Replica_SQL_Running'
echo "=== Redis 内存 ==="
RP=$(grep '^REDIS_PASSWORD=' /opt/cooking-platform/.env.prod | cut -d= -f2-)
docker exec cooking-redis-master-prod redis-cli -a "$RP" --no-auth-warning INFO memory | grep used_memory_human
echo "=== RabbitMQ 队列 ==="
docker exec cooking-rabbitmq-prod rabbitmqctl list_queues name messages messages_ready
EOF
```

---

## 5. 故障排查(CLAUDE.md §9 浓缩 + 实战补充)

### 5.1 `api.mellowck.com` 502 / 503

```bash
# 1. app 容器健康?
docker ps --filter name=cooking-app --format "{{.Names}}\t{{.Status}}"

# 2. nginx 能否到 app(从 nginx 容器内)
docker exec cooking-nginx-prod sh -c "wget -qO- http://app1:8080/health; echo; wget -qO- http://app2:8080/health"

# 3. app 日志(prod log.console:false,docker logs 是空的,看文件)
docker exec cooking-app1-prod sh -c "grep -E 'ERROR|FATAL' /var/log/cooking/app.log | tail -30 | jq ."

# 4. nginx error log
docker exec cooking-nginx-prod tail -30 /var/log/nginx/error.log

# 5. upstream 是不是被熔(被动健康检查触发 next_upstream)
docker exec cooking-nginx-prod tail -30 /var/log/nginx/access.log | awk '{print $9, $10, $NF}' | sort | uniq -c
```

最常见:DB 池满 → app SQL 排队 → 5xx 飙;参 §5.6。

### 5.2 CORS 没回 Allow-Origin

```bash
curl -i -X OPTIONS https://api.mellowck.com/api/v1/feed \
  -H 'Origin: https://mellowck.com' \
  -H 'Access-Control-Request-Method: GET' | grep -i 'access-control-allow-origin'
# 期望:access-control-allow-origin: https://mellowck.com
```

无 → 看容器内是不是新版 yaml:
```bash
docker exec cooking-app1-prod grep -A 4 '^cors:' /app/configs/config.prod.yaml
# 不是 mellowck.com → 改了 yaml 但忘 --build,重跑:
docker compose ... up -d --build --force-recreate app1 app2
```

### 5.3 证书续签失败 / api. 不通

```bash
certbot certificates                 # 看到期日
certbot renew --dry-run              # 模拟,看错误

# 常见:80 端口 ACME 路径不通 → 检查 nginx :80 server_name 包含 api.mellowck.com
docker exec cooking-nginx-prod grep -A 3 'listen 80' /etc/nginx/nginx.conf
```

续了但 nginx 没 reload → 看 `/etc/letsencrypt/renewal-hooks/deploy/*.sh`,缺则补:
```sh
#!/bin/sh
docker exec cooking-nginx-prod nginx -s reload   # cert 是目录挂载,reload 有效
```

### 5.4 nginx.conf 改了不生效

**根因**:单文件 bind mount + git pull → inode 换了,容器持旧 fd → reload 无效。**必须** `--force-recreate nginx`,见 §4.2。验证 sha 一致即可。

### 5.5 prod 日志找不到(看 docker logs 是空的)

**预期**——prod `log.console: false`,stdout 故意零输出。看文件:
```bash
docker exec cooking-app1-prod tail -f /var/log/cooking/app.log
docker exec cooking-app1-prod sh -c "grep ERROR /var/log/cooking/app.log | jq ."
```

zap JSON 字段:`level / ts / caller / msg / request_id / user_id / phone` 等,按需 `jq` 过滤。

### 5.6 MySQL 连接池接近耗尽 / slave 落后

```bash
RPW=$(grep '^MYSQL_ROOT_PASSWORD=' .env.prod | cut -d= -f2-)

# 看主库 PROCESSLIST(找慢查询)
docker exec cooking-mysql-prod mysql -uroot -p"$RPW" -e "SHOW PROCESSLIST" | awk '$6 > 5'

# 看 max_connections / 当前连接
docker exec cooking-mysql-prod mysql -uroot -p"$RPW" -e \
  "SHOW VARIABLES LIKE 'max_connections'; SHOW STATUS LIKE 'Threads_connected';"

# 看 slave 状态
docker exec cooking-mysql-slave-prod mysql -uroot -p"$RPW" -e "SHOW REPLICA STATUS\G" \
  | grep -E 'Seconds_Behind_Source|Last_Error|Replica_IO_Running|Replica_SQL_Running'
```

`Seconds_Behind_Source > 5` 持续 → app 读自己刚写的场景会读到旧数据 → 临时把读流量切回 master(改 DBResolver Policy)或排查 slave 长事务。

### 5.7 Redis 连接打满 / Sentinel 选主

```bash
RP=$(grep '^REDIS_PASSWORD=' .env.prod | cut -d= -f2-)
docker exec cooking-redis-master-prod redis-cli -a "$RP" --no-auth-warning INFO clients
docker exec cooking-redis-master-prod redis-cli -a "$RP" --no-auth-warning INFO replication
```

Sentinel 选主期间 app 短暂报错 → 看 `internal/cache/` 是否有 retry。当前是旁路缓存,Redis 短暂挂只影响命中率,DB 兜底。

### 5.8 RabbitMQ Consumer backlog 持续 > 100

```bash
docker exec cooking-rabbitmq-prod rabbitmqctl list_queues name messages messages_ready consumers
```

- backlog 涨 + consumers 数为 0 → app 容器内 consumer goroutine 死了,重启 app
- backlog 涨 + consumers 正常 → 单条 consumer 慢,看 app 日志找瓶颈
- DLX 队列 `xxx.dlq` 涨 → 业务消息持续失败,看 `app.log` 里 `consumer.*error` 字段

监控:Prometheus 已 scrape RabbitMQ Management,Grafana 有"Consumer backlog"看板。

### 5.9 app1/app2 反复重启 / OOM

```bash
docker ps --filter name=cooking-app --format "{{.Names}}\t{{.Status}}"
docker stats --no-stream cooking-app1-prod cooking-app2-prod
docker inspect cooking-app1-prod --format '{{json .State}}' | jq '.OOMKilled, .ExitCode, .Error'
```

OOMKilled=true → 调 compose 的 mem_limit 或排查内存泄漏(`pprof` 端点:`http://app1:8080/debug/pprof/`,**内网 only**)。

### 5.10 Grafana 上不去

Grafana 只绑 loopback(`127.0.0.1:3000`)。本地 Mac 起隧道:
```bash
ssh -L 3000:127.0.0.1:3000 -i ~/.ssh/id_cooking_platform root@47.238.29.251
# 浏览器 http://localhost:3000,密码看 .env.prod 的 APP_GRAFANA_ADMIN_PASSWORD
```

### 5.11 5xx 比例 > 1% 持续

Grafana "HTTP 延迟趋势"看板 → 看是哪个 endpoint。然后:
1. `docker exec cooking-app1-prod sh -c "grep -E 'status\":50' /var/log/cooking/app.log | jq -r '.path' | sort | uniq -c | sort -rn | head"`
2. 走 §5.1 / §5.6 / §5.8 排具体原因

---

## 6. SSH + Docker 速查

```bash
# 跳板
ssh -i ~/.ssh/id_cooking_platform root@47.238.29.251
cd /opt/cooking-platform

# 全栈状态
docker compose -f docker-compose.prod.yml --env-file .env.prod ps

# app 日志
docker exec cooking-app1-prod tail -f /var/log/cooking/app.log
docker exec cooking-app2-prod tail -f /var/log/cooking/app.log

# nginx access log(看实际流量分布)
docker exec cooking-nginx-prod tail -f /var/log/nginx/access.log

# 进容器(app 用 distroless,没 shell;mysql/redis/rabbitmq 都能进)
docker exec -it cooking-mysql-prod bash
docker exec -it cooking-redis-master-prod sh

# 资源占用
docker stats --no-stream

# 网络拓扑
docker network inspect cooking-platform_cooking-prod-net

# 全栈重启(慎用)
docker compose -f docker-compose.prod.yml --env-file .env.prod restart
```

---

## 7. 回滚

### 7.1 Go 代码回滚

```bash
git log --oneline -10
git revert <bad-sha>     # 创建反向 commit(推荐)
# 或本地 reset 后强推(危险,慎)
git push origin main

# 服务器
cd /opt/cooking-platform && git pull
docker compose ... up -d --build --force-recreate app1 app2
```

### 7.2 配置 yaml 回滚

```bash
cd /opt/cooking-platform
git checkout HEAD~1 -- configs/config.prod.yaml
docker compose ... up -d --build --force-recreate app1 app2
```

### 7.3 nginx.conf 回滚

```bash
# 优先用 §4.2 的 .bak.YYYYMMDD 备份
cp deploy/nginx/nginx.conf.bak.20260531-130000 deploy/nginx/nginx.conf
docker compose ... up -d --force-recreate nginx
sha256sum deploy/nginx/nginx.conf
docker exec cooking-nginx-prod sha256sum /etc/nginx/nginx.conf
```

### 7.4 数据库 migration 回滚

```bash
make migrate-down       # 回滚最后一个 migration
# 多个连退:
make migrate-down && make migrate-down
```

⚠️ 不可逆的 migration(DROP COLUMN)down 不回数据。备份策略:大改前 `mysqldump`:
```bash
RPW=$(grep '^MYSQL_ROOT_PASSWORD=' .env.prod | cut -d= -f2-)
docker exec cooking-mysql-prod mysqldump -uroot -p"$RPW" cooking > /opt/backup/cooking-$(date +%Y%m%d-%H%M).sql
```

### 7.5 证书回滚

证书签了就签了,不会回滚——多签一张不影响旧域名使用。删除:
```bash
certbot delete --cert-name api.mellowck.com
```

---

## 8. 已知坑(踩过的,别再踩)

| 坑 | 现象 | 原因 | 解 |
|---|---|---|---|
| `nginx.conf` 单文件 bind mount + `reload` | 改了 nginx.conf 容器内行为不变 | 宿主机文件 inode 换了,容器持旧 fd,reload 无效 | **必须** `up -d --force-recreate nginx`(CLAUDE.md §9.2) |
| `configs/*.yaml` 是 `COPY` 进镜像 | 改 prod yaml 后 `--force-recreate` 不生效 | Dockerfile `COPY configs/`,普通 recreate 用旧 image | `up -d --build --force-recreate app1 app2`(2026-05-31 CORS 修复时踩过) |
| Viper 嵌套 / 数组 env 覆盖 | `APP_CORS_ALLOWED_ORIGINS=xxx` 不生效 | Viper AutomaticEnv 对 yaml 数组覆盖行为有限 | 改 yaml + rebuild;简单 string key 才用 env 覆盖(见 `docs/progress/19_*.md`) |
| `docker logs` 看 app 是空的 | 看不到任何 Go 日志 | prod `log.console: false`(故意,降磁盘 IO) | 看文件 `docker exec ... tail -f /var/log/cooking/app.log`(CLAUDE.md §9.4) |
| `curl -sI` HEAD 误判 404 | `curl -sI /health` 返 404,浏览器 GET 又 200 | Gin `r.GET` 不响应 HEAD,`-I/-sI` 是 HEAD | 验证用 `curl -s -i`(GET + 显示头)(CLAUDE.md §9.5) |
| `.env.prod` 权限漂回 644 | 旁路用户能 `cat` 出全部凭证 | scp / cp / git stash 意外重置权限 | 月度巡检 verify(本文 §4.4),按需 `chmod 600` |
| `/metrics` 被暴露 | 公网能看到 goroutine / GC / build 信息 | nginx 漏挡 / 加新 location 时忘 deny | 红线 §10.3:nginx 必须 `location = /metrics { return 404; }`,api. 和 mellowck. 都要 |
| SMS per-IP 限流 prod 验证脚本失败 | `verify-step20` 抓码失败 | Redis `sms:ip:*` 累积达阈值 | `redis-cli --scan --pattern 'sms:ip:*'` + DEL(CLAUDE.md §9.3) |
| 主域 `/health` 404 | 老外部探针(阿里云监控/blackbox)报警 | 阶段 2 后 mellowck. 归前端 Next.js | 外部探针 URL 切到 `api.mellowck.com/health`(2026-05-31 阶段 2 行为变化) |
| Prometheus scrape app1/app2 配错 → /metrics 不通 | Grafana 没数据 | scrape target 写错 / app 端口错 | 看 `http://prometheus:9090/targets`(SSH 隧道访问) |

---

## 9. 凭证 / 联系

| 物 | 在哪 |
|---|---|
| SSH 私钥 | 本地 `~/.ssh/id_cooking_platform` |
| `.env.prod` 真实凭证 | 服务器 `/opt/cooking-platform/.env.prod`(600 root:root)+ 本地 `docs/ops/prod.md`(唯一备份) |
| GitHub 仓库 | `git@github.com:quqxiaoli/cooking-platform.git`(Private) |
| GitHub Deploy Key | 服务器端独立生成,不跨机器搬运 |
| 阿里云 ECS 控制台 | aliyun.com,实例 `47.238.29.251`(HK,2C4G) |
| 阿里云 RAM 子账号 | `cooking-platform-oss`(OSS / SMS / Audit)AK/SK 在 `.env.prod` |
| 阿里云 OSS bucket | `cooking-platform-prod`(HK,公共读 + CORS allow `mellowck.com`) |
| 阿里云短信签名 | 控制台短信服务 → 国内消息 |
| 阿里云内容安全 | 控制台内容安全 → 文本 / 图片审核 |
| 域名 / DNS | 阿里云万网,`mellowck.com`(自动续费已开;A 记录 @/www/api 三条) |
| Let's Encrypt 邮箱 | `y1756062305@gmail.com`(过期前会通知) |
| Grafana 密码 | `.env.prod` 的 `APP_GRAFANA_ADMIN_PASSWORD` |
| Alertmanager 邮箱 | `.env.prod` 的 `ALERT_EMAIL_TO` |

---

## 10. 何时回 CLAUDE.md / RETROSPECTIVE vs 本文

| 场景 | 看哪 |
|---|---|
| 半夜出事排障 | **本文 §5** |
| 改 prod 配置 / 代码 / nginx | **本文 §4** |
| 想知道某决策的来龙去脉(为什么 GTID? 为什么 DBResolver?) | `docs/project/PROJECT_RETROSPECTIVE.md` |
| 想看完整架构原则 / 红线 / 偏离点登记规则 | `CLAUDE.md` §4 §6 §10 |
| 新功能开工前的规划 | `CLAUDE.md` §6.1(二步开工契约) |
| 数据库 schema / 字段语义 | `docs/api-spec/` + migration 文件 |
| 前端那边出事 | `docs/ops/FRONTEND_OPS_RUNBOOK.md`(前端 RUNBOOK) |
| 2026-05-31 域名拆分上线证据 | `docs/handoff/backend-deploy-output.md` |
| 历史 step 进度 | `docs/progress/NN_项目进度追踪.md` |
| 遗留债务清单 | `CLAUDE.md` §11 |
