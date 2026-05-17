# 代码变更清单 · 第 15 步 · Nginx 双实例负载均衡

---

## 基础设施层（新增）

- `Dockerfile`（新增）
  · 多阶段构建：Stage 1（golang:1.25-alpine）编译静态二进制，Stage 2（alpine:3.21）只打入 binary + configs/。`CGO_ENABLED=0 GOOS=linux` 保证无 glibc 依赖，`-trimpath -ldflags="-s -w"` 裁掉调试信息。

- `configs/config.docker.yaml`（新增）
  · 覆盖 Docker 内网连接地址：`mysql:3306`、`mysql-slave:3306`、`redis:6379`、`rabbitmq:5672`。其余字段与 config.yaml dev 默认值相同（含 jwt.secret dev 值）。`mq.provider=rabbitmq`，因 Docker 环境 RabbitMQ 已在网络内可用。

- `deploy/nginx/nginx.conf`（新增）
  · upstream `cooking_backend`：server app1:8080 + app2:8080，`keepalive 32`。
  · `proxy_http_version 1.1; proxy_set_header Connection "";` — 两行必须同时出现才能激活 upstream keepalive（HTTP/1.0 默认 Connection: close 会关闭连接）。
  · `proxy_next_upstream error timeout http_503` — passive health check，open-source Nginx 唯一可用方式。
  · 日志格式追加 `upstream=$upstream_addr`，供 verify 脚本统计 round-robin 分布。

---

## Docker Compose 层（修改）

- `docker-compose.yml`（修改：追加 app1、app2、nginx 三个服务）
  · app1 / app2：`build: { context: ., dockerfile: Dockerfile }`；env `CONFIG_PATH=configs/config.docker.yaml`；depends_on mysql + mysql-slave + redis + rabbitmq（均 service_healthy）；healthcheck `wget -qO- http://localhost:8080/health/ready`；不暴露 host port（只通过 nginx 对外）。
  · **没有依赖 mysql-slave-init**：重新 `docker compose up -d` 时 Compose 会重建 one-shot 容器，第二次 init 遭遇 super_read_only=ON 导致 exit 1。App 只需 MySQL 容器 healthy，不需要 replication 已完成初始化。
  · nginx：`image: nginx:1.27-alpine`，port `80:80`，挂载 `./deploy/nginx/nginx.conf:ro`；depends_on app1 + app2 healthy。

---

## 配置 / Config 层（修改）

- `pkg/config/config.go`（修改：Load() 追加 CONFIG_PATH 支持）
  · 在 `v.SetConfigName/AddConfigPath` 之前检查 `os.Getenv("CONFIG_PATH")`；非空则 `v.SetConfigFile(path)` 直接指定配置文件路径，Viper 从文件扩展名推断格式。
  · 设计意图：三套配置文件（dev / docker / prod）通过环境变量路径切换，不改代码；避免 Viper 对 `[]string` 型字段 env var 覆盖行为不一致的问题。

---

## 接入层（修改）

- `cmd/server/main.go`（修改：setupRouter 追加 `/health/ready` 路由）
  · `r.GET("/health/ready", healthHandler.Readiness)` — Nginx upstream health check 专用别名。
  · 原 `/readiness` 路由保留，向后兼容所有现有脚本和监控。

---

## 收尾

- `scripts/verify_step15.sh`（新增）
  · §1 编译；§2 容器 healthy；§3 `/health/ready` 200；§4 `/readiness` 向后兼容；§5 100 次请求 round-robin 分布验证（用行数差值法从 `docker logs` 截取本次请求日志，规避 access.log→/dev/stdout 符号链接导致的 cat 阻塞）；§6 `nginx -t` 语法检查。

- `Makefile`（修改：追加 verify-step15 target）
