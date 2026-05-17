# 故事线 · 第 15 步 · Nginx 双实例负载均衡

---

## Part A · 本步故事线

### 起点

进入 Step 15 时，项目已完成 MySQL 主从 + DBResolver 读写分离（Step 14）。Go 服务以 `go run` 方式在宿主机跑，直连 `127.0.0.1:3306/6379/5672`；docker-compose.yml 里只有数据库 / Redis / RabbitMQ 三层基础设施，没有任何应用容器。这一步要解决的问题是：**让两个 Go 实例跑在 Docker 内，对外由 Nginx 统一分发流量**，为后续 Prometheus 监控和 GitHub Actions CI/CD 奠定容器化基础。

### 设计思考

**决策一：如何给容器传内网连接地址？**

容器内不能用 `127.0.0.1:3306`，要改成 `mysql:3306`（Docker 服务名）。有两条路：
- **方案 B**：`docker-compose.yml` 里注入 `APP_DATABASE_DSN`、`APP_REDIS_ADDR` 等 env var，Viper `AutomaticEnv()` 覆盖。问题在于 `SlavesDSN` 是 `[]string`，Viper 对数组型字段的 env var 覆盖没有官方规范（社区方案有 JSON 序列化、逗号分隔等，各版本行为不一致），极易踩坑且面试难以自洽解释。
- **方案 A**（最终选择）：新增 `configs/config.docker.yaml`，只覆盖连接地址；在 `config.Load()` 里检查 `CONFIG_PATH` env var，若存在则 `v.SetConfigFile()` 直接读取指定文件。三套配置（`config.yaml` dev 直连 / `config.docker.yaml` Docker 内网 / `config.prod.yaml` 生产占位）分层清晰，新人一眼能看懂，也是面试中"配置分层"的标准答案。

这个决策同时让 `config.Load()` 获得了扩展性：以后 staging / blue-green 环境只需挂不同的配置文件，代码零改动。（PRD v3.0 §6 配置项全表需更新）

**决策二：Nginx active vs passive health check**

conventions 文档写的是"health check 路由 `/health/ready`"，一开始以为要配置 Nginx 主动探活。查文档后确认：Nginx 开源版（nginx.org）只有 passive health check，即请求失败时才踢出 upstream；active health check（定期发探针）是 nginx-plus 的商业功能，或需要 `ngx_http_upstream_check_module` 这个第三方模块重新编译 Nginx。

选择：passive health check（`proxy_next_upstream error timeout http_503`）+ 对 app1/app2 容器配置 Docker healthcheck（`wget /health/ready`），让容器编排层而非 Nginx 层做存活判断。这是开源方案的标准做法，登记为偏离点。

**决策三：`/health/ready` 路由**

现有代码已有 `/health`（liveness）和 `/readiness`（readiness），但 conventions 要求 `/health/ready` 作为 Nginx 专用路径。取舍：新增别名路由而非重命名——重命名会破坏 Step 1 以来所有使用 `/readiness` 的脚本和监控，别名只是一行 `r.GET("/health/ready", healthHandler.Readiness)`，零破坏。

### 实现节奏

**先写 Dockerfile**，多阶段构建（builder→runner）是容器化的第一步，也是最核心的。细节：`CGO_ENABLED=0 GOOS=linux` 保证静态链接，`-trimpath -ldflags="-s -w"` 裁掉调试信息减小镜像体积，`COPY configs/ ./configs/` 把所有配置文件打入镜像，让 `CONFIG_PATH` 切换配置而非挂载覆盖。

**然后写 nginx.conf**，核心是三点：
1. `upstream cooking_backend { server app1:8080; server app2:8080; keepalive 32; }` — 服务名由 Docker DNS 解析；
2. `proxy_http_version 1.1; proxy_set_header Connection "";` — 这两行配合 `keepalive 32` 才真正生效（HTTP/1.0 默认 Connection: close，会使 keepalive 失效）；
3. 日志格式追加 `upstream=$upstream_addr`，为 verify_step15.sh 的分布验证提供数据来源。

**docker-compose.yml 追加三个服务**。踩坑发生在这里：

### 踩坑记录

**坑：mysql-slave-init 被重建后 exit 1。**

首次 `docker compose up -d` 正常。加入 app1/app2/nginx 后再次 `docker compose up -d`，Compose 认为配置文件有变动，重建了 `mysql-slave-init` 容器。第二次运行 `init-slave.sh`：Step 1 mysqldump 成功，Step 2 STOP REPLICA 成功，Step 3 恢复 dump 时失败——因为第一次运行结尾执行了 `SET GLOBAL super_read_only=ON`，现在 slave 拒绝任何写入，包括 root 用户的 dump restore。

根因：`restart: "no"` 阻止的是容器崩溃后的自动重启，但 `docker compose up -d` 判断"服务定义有变化"（新 depends_on 引用）时仍会重建。

修复：从 app1/app2 的 `depends_on` 中移除 `mysql-slave-init`。App 只需要 mysql 和 mysql-slave 容器健康即可启动——DBResolver 会把写流量送 master、读流量送 slave，两者都 healthy 就够了。Replication 状态是基础设施运维的职责，不是应用进程的启动前提。

**坑：验证脚本里 `docker exec cat /var/log/nginx/access.log` 阻塞。**

写好验证脚本后，§5 卡死不返回。排查：nginx 官方 Docker 镜像把 `/var/log/nginx/access.log` 做成了 `/dev/stdout` 的符号链接——`docker exec cat` 会读 `/dev/stdout`，而容器的 stdout 是一个 pipe，没有 EOF，所以永远阻塞。

修复：改用 `docker logs cooking-nginx-dev` 读取实际日志（输出到 Docker log driver），用行数差值法（`lines_before` → send → `lines_after`，`tail -n new_lines`）精确截取本次请求产生的日志行。

**坑：`truncate -s 0` 在 BusyBox 里不支持。**

最初用 `docker exec ... sh -c "truncate -s 0 /var/log/nginx/access.log"` 清空日志文件，在 Alpine BusyBox 的 `truncate` 实现里 `-s 0` 不被支持（BusyBox truncate 不实现这个参数）。改用 `: > /var/log/nginx/access.log`——但随后发现 access log 本来就是 stdout 符号链接，清空操作本身也是多余的，最终整体改为行数差值法后这个坑自然消失。

### 收获

这一步最大的收获是对 **Docker 容器化陷阱** 的理解：
1. 镜像内的"文件"不一定是真正的文件（access.log → /dev/stdout）；
2. `restart: "no"` ≠ "compose up 不重建"，一次性容器的幂等性必须在脚本层保证；
3. Nginx keepalive 需要 `proxy_http_version 1.1 + Connection: ""` 配合，否则只是在配置里写了 keepalive 但实际每次请求都新建 TCP 连接。

对后续步骤的伏笔：Step 16 Prometheus 需要在 Go 容器里暴露 `/metrics` 端点，Nginx 可以选择透传或屏蔽——屏蔽更安全（metrics 不对外暴露），需要在 nginx.conf 里加 `location /metrics { deny all; }`。

---

## Part B · 关联知识点清单

**基础概念（语言 / 框架层面）**
- Nginx upstream 模块：server weight、max_fails、fail_timeout 参数
- HTTP/1.1 persistent connection vs HTTP/1.0 Connection: close
- Nginx passive vs active health check（开源版限制）

**设计模式与架构思想**
- 配置分层策略：dev / docker / prod 三套配置文件 + 环境变量路径切换
- 一次性初始化容器（one-shot init container）的幂等性设计

**Go 语言特性与并发模型**
- `CGO_ENABLED=0` 静态链接的意义（alpine 无 glibc）
- Viper `AutomaticEnv()` 对数组型字段的局限性

**工程实践（测试 / 部署 / 可观测性等）**
- Docker 多阶段构建（multi-stage build）减小镜像体积
- Docker Compose `depends_on` condition 类型：`service_healthy` vs `service_completed_successfully`
- Nginx Docker 镜像的 stdout/stderr 软链接约定
- `docker logs --since` 时间戳格式（RFC3339）及行数差值法替代方案

**数据安全与合规**
- （无）

**可能被延伸追问的关联领域**
- 如何做 blue-green 部署（Nginx upstream 动态切换）
- Nginx upstream keepalive 与连接池的对比
- 容器健康检查（Docker healthcheck）与 Kubernetes liveness/readiness probe 的异同
