# CLAUDE.md · cooking-platform 项目指南

> 这是 Claude Code 在 cooking-platform 仓库中工作的项目上下文文件。
> 每次会话开始时 Claude Code 会自动读取本文件，理解项目当前状态、架构原则和维护规范。
>
> **本文档版本**：v1.0（项目收官后首版）
> **最后更新**：2026-05-20
> **维护责任人**：小离

---

## §1 项目当前状态

**cooking-platform** 是一款已上线公网的烹饪内容分享平台后端，目标用户 22-28 岁独居/合租年轻人，按场景（出租屋/一个人的饭/露营野炊/家庭厨房/快手日常/打包便当/减脂餐/节气节日）组织内容。

| 项 | 状态 |
| --- | --- |
| **生产域名** | https://mellowck.com |
| **服务器** | 阿里云香港 ECS（47.238.29.251），2C4G，Ubuntu 22.04 |
| **项目阶段** | ✅ **已正式收官**（20/20 步完成，进入维护期） |
| **最后一步** | Step 20 公网验证 + 压测（2026-05-20） |
| **架构 KPI 实证** | slave 读流量占比 91%、HTTPS A+、端到端 P99 ≤ 40ms |

**本项目不再有"下一步开发任务"**。所有新工作要么是维护型（证书续期、监控响应、安全补丁），要么是迭代型（启动新版本规划，进入 cooking-platform v2 范畴）。

---

## §2 必读文档（按优先级）

修改任何代码前，Claude Code 必须先读以下文档：

| 优先级 | 文档 | 用途 |
| --- | --- | --- |
| P0 | `docs/prd/PRD-Final-v3.0.md` | 最终权威 PRD，接口契约 / 错误码 / 架构决策的唯一真相源 |
| P0 | `docs/PROJECT_RETROSPECTIVE.md` | 项目总复盘，包含 12 个关键技术决策的完整背景 |
| P1 | `docs/progress/18_项目进度追踪.md` | Step 18 服务器+域名+HTTPS 全过程 |
| P1 | `docs/progress/19_项目进度追踪.md` | Step 19 数据库迁移+部署联调（含 Viper 嵌套 env 排查复盘） |
| P1 | `docs/progress/20_项目进度追踪.md` | Step 20 公网验证 + 压测 |
| P2 | `docs/storylines/` 下所有故事线 | 工程师视角的踩坑与决策叙事 |

---

## §3 技术栈（不可更改）

| 层 | 选型 |
| --- | --- |
| 语言/框架 | Go 1.23 + Gin |
| ORM | GORM + DBResolver（读写分离 Random Policy） |
| 主数据库 | MySQL 8.0（GTID 主从复制） |
| 缓存 | Redis 7.2（Sentinel 模式） |
| 消息队列 | RabbitMQ（持久化 + ACK + DLX）；通过 `EventBus interface` 抽象，dev 用 Channel、prod 用 RabbitMQ |
| 图片存储 | 阿里云 OSS（PresignedURL 客户端直传，香港 Bucket） |
| 短信 | 阿里云短信（生产）/ Mock（dev） |
| 内容审核 | 阿里云内容安全 Green API（生产）/ Mock auto-pass（dev） |
| 部署 | Docker Compose（双 Go 实例 + Nginx + MySQL 主从 + Redis + RabbitMQ） |
| 监控 | Prometheus + Grafana（端口 3000，只绑 loopback，需 SSH 隧道访问） |
| CI/CD | GitHub Actions（PR workflow + verify-step17） |
| 数据库迁移 | golang-migrate |
| HTTPS | Let's Encrypt（certbot webroot 模式）+ Mozilla Intermediate + HSTS 1 年 |

---

## §4 关键架构原则（红线）

修改代码时必须遵守。任何偏离都要在 commit message 中显式说明并在 PR description 中标注"偏离点登记"。

### 4.1 单体优先，未来可拆

`cmd/server/main.go` 是唯一入口。所有模块通过依赖注入装配，不引入服务发现 / 分布式追踪 / 分布式事务。预留拆分能力：`service` 层之间通过 `EventBus` 而不是直接调用解耦。

### 4.2 三层架构 + 旁路缓存 + 异步事件

```
internal/handler/    ← bind → validate → service → response（不写业务逻辑）
internal/service/    ← 业务逻辑（不直接操作 DB / Redis）
internal/repository/ ← DB 操作（接口先行，ErrXxxNotFound 封装）
internal/cache/      ← Redis 操作（旁路缓存，不是穿透式）
internal/consumer/   ← 异步事件消费者
internal/event/      ← EventBus 接口 + 两套实现
```

**禁止**：
- handler 直接调 repository（必须经过 service）
- service 嵌入 SQL（必须经过 repository）
- repository 调用其他模块的 service（会导致循环依赖）

### 4.3 接口先行 + Mock + Real 三件套

每个外部依赖必须按以下模式实现：

```
pkg/<dep>/
├── <dep>.go        ← interface 定义
├── mock.go         ← Mock 实现
└── aliyun.go       ← 真实实现
```

配置切换通过 `<dep>.provider: mock|aliyun`，零业务代码改动。当前已落地：`pkg/sms/`、`pkg/audit/`、`pkg/oss/`。

### 4.4 错误码段位预分配（不可重新分配）

| 段位 | 模块 | 已用范围示例 |
| --- | --- | --- |
| 400000-409999 | 通用 | 400001 / 401001-401003 / 412101-412104 / 429001 |
| 41YZZZ | 用户 | 410001-410999 |
| 42YZZZ | 内容 | 420001-420999 |
| 43YZZZ | 互动（点赞/PV） | 430001-430999 |
| 44YZZZ | 关注 | 440001-440999 |
| 45YZZZ | 搜索 | 450001-450999 |
| 46YZZZ | 上传 | 460001-460999 |
| 47YZZZ | 审核 | 470001-470999 |
| 48YZZZ | 加密 | 480001-480103 |
| 503XXX | HTTP 5xx | 503001 |

新增错误码必须追加到 `pkg/errcode/errcode.go` 文件末尾，不修改已有定义。

### 4.5 永久排除的功能

| 功能 | 排除理由 |
| --- | --- |
| **评论** | 产品决策，永久不做。评论显著增加内容审核成本、放大社区负能量、与"不焦虑"产品气质相悖。正反馈闭环走点赞。 |
| **私信** | 超出 MVP 范围，三期后单独评估。 |
| **直播** | 不在产品定位范围内。 |
| **付费内容** | 二期评估。 |

视频内容是延期不是排除，二期评估时再决定。

---

## §5 已完成文件的修改边界

### 5.1 扩展性变更白名单（无需特别豁免）

| 文件 | 允许的修改 |
| --- | --- |
| `cmd/server/main.go` | 添加新模块的依赖装配与路由注册 |
| `configs/config.yaml` + `config.docker.yaml` + `config.prod.yaml` | 新增配置段（必须三 yaml 同步） |
| `pkg/config/config.go` | 新增配置结构体并挂到 Config（不改已有字段） |
| `pkg/errcode/errcode.go` | 文件末追加新错误码（不改已有定义） |
| `Makefile` | 追加新命令（不改已有命令） |
| `go.mod` / `go.sum` | `go get` 新依赖 |

### 5.2 需要显式豁免的修改

上述白名单之外的已完成文件，如要修改必须在 PR description 中：
1. 在"假设清单"章节显式列出
2. 说明为什么不能用扩展方式实现
3. 等小离明确豁免后才能动

---

## §6 v3 工作流（人机协作规范）

### 6.1 二步开工契约

每个新任务（新功能 / bug 修复 / 重构）开工时按二步走：

**第 1 步**：100 字说明 + 主动 fetch 文件
- 100 字以内说明要做什么、为什么
- 列出要读的文件清单（精确到路径，不写通配符）
- 通过 GitHub MCP 或本地 `view` 逐一读取
- 单任务读取 ≤ 15 个文件，超出分批

**第 2 步**：假设清单 + 批量化生成
- 列出会用到的：函数签名、结构体字段、配置项、错误码段位、Redis Key、MQ Topic
- 假设清单基于已读代码，禁止凭推测
- 用户确认或纠正后按子模块分组生成
  - 数据层：migration + model + repository（含 interface）
  - 业务层：service + cache（如有）
  - 接入层：handler + DTO + 路由装配
  - 收尾：main.go 装配 + verify 脚本 + Makefile 命令

### 6.2 GitHub MCP 使用纪律

- **读优先**：每次任务开工先 fetch 真实代码，再写假设清单
- **写禁止**：Claude Code 不通过 MCP 写仓库（不创建 PR、不直接 commit）；所有写操作由小离本地执行
- **避免重复 fetch**：本次会话已 fetch 过的文件不再重复
- **隐私边界**：不主动 fetch `.env`、`configs/*.prod.yaml`（含真实 AK）、`secrets/`、`.ssh/`，除非小离明确要求

### 6.3 偏离点登记机制

任何与 PRD 不一致的实现必须登记到对应步骤的进度文件"本步与 PRD 的偏离点"章节，格式：

```markdown
| PRD 章节 / 规则 | 实现差异 | 选择此方案的原因 |
| --- | --- | --- |
| PRD-Phase3 §X.Y | 具体差异 | 为什么这样选 |
```

偏离点是后续生成新版 PRD 的唯一权威依据。

---

## §7 常用命令清单

### 7.1 Make targets

```bash
# 构建与基础检查
make build                  # go build ./...
make test                   # go test ./... -race
make lint                   # go vet + staticcheck
make check-config-parity    # 三 yaml 顶级 key 集合一致性

# 验证脚本
make verify-step7           # 搜索模块端到端验证（dev）
make verify-step8           # 关注模块
# ... 各 step 都有对应 verify
make verify-step17          # CI 流水线自检（静态检查 + 文件存在）
make verify-step20          # prod 端到端验证 + 主从读写分离实证（需 MYSQL_ROOT_PW）

# 压测
make stress-test            # Step 20 公网压测（需 wrk2 + MYSQL_ROOT_PW）

# 数据库迁移
make migrate-up             # 应用未跑的 migration
make migrate-down           # 回滚最后一个
make migrate-create NAME=xxx  # 生成新的 migration 文件对

# 收尾自检（v4 工作流工具）
make step-closeout N=20     # 检查 step N 收尾交付物完整性
make step-diff              # 当前分支与 main 的 diff 摘要
```

### 7.2 git 工作流

```bash
# 启动新任务（任务编号 N，模块名 mod）
git checkout main
git pull origin main
git checkout -b feature/v2-N-<mod>

# ... 在 feature 分支上开发、commit ...

# 收尾合并
git checkout main
git merge --no-ff feature/v2-N-<mod>
git tag v2-N-done
git push origin main --tags
```

### 7.3 prod 运维（在服务器 47.238.29.251）

```bash
# 进项目目录
cd /opt/cooking-platform

# 拉新代码
git pull origin main

# 重启某个服务（保持配置卷挂载、首次 inode 失效兜底）
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --force-recreate <service>

# 看服务状态
docker compose -f docker-compose.prod.yml --env-file .env.prod ps

# 看实时日志（app 容器内部，因 prod log.console:false）
docker exec cooking-app1-prod tail -f /var/log/cooking/app.log
docker exec cooking-app2-prod tail -f /var/log/cooking/app.log

# 看 nginx access log（在 docker logs 里）
docker logs --tail 50 cooking-nginx-prod

# 看主从复制状态
RPW=$(grep '^MYSQL_ROOT_PASSWORD=' .env.prod | cut -d= -f2-)
docker exec cooking-mysql-slave-prod mysql -uroot -p"$RPW" -e "SHOW REPLICA STATUS\G" | grep -E 'Seconds_Behind_Source|Replica_IO_Running|Replica_SQL_Running'

# Redis 巡检
docker exec cooking-redis-prod redis-cli -a "$(grep '^REDIS_PASSWORD=' .env.prod | cut -d= -f2-)" --no-auth-warning INFO memory | grep used_memory_human
```

### 7.4 Grafana 访问（只绑 loopback，需 SSH 隧道）

```bash
# 在本地 Mac 上跑（保持窗口打开）
ssh -L 3000:127.0.0.1:3000 -i ~/.ssh/id_cooking_platform root@47.238.29.251

# 浏览器访问 http://localhost:3000
# 默认密码见服务器 .env.prod 的 GF_SECURITY_ADMIN_PASSWORD
```

---

## §8 维护操作 SOP

### 8.1 每月一次

```bash
# 1. HTTPS 证书检查（host certbot 自动续期，但人工 verify 是好习惯）
ssh root@47.238.29.251 'certbot certificates'
# 看到期日，距离 < 30 天才需要人工干预

# 2. 服务器磁盘
ssh root@47.238.29.251 'df -h && du -sh /var/lib/docker/volumes/*'

# 3. Grafana 巡检（SSH 隧道 + 浏览器）
# 关注 4 块看板：HTTP 延迟趋势 / Consumer backlog / MySQL 池利用率 / Redis 命中率

# 4. 日志容量（避免无限增长）
ssh root@47.238.29.251 'docker exec cooking-app1-prod du -sh /var/log/cooking/'
# > 1G 时考虑加 logrotate
```

### 8.2 每季度一次

```bash
# 1. 依赖升级 review
go list -u -m all  # 看哪些依赖有新版

# 2. Docker base image 升级 review
grep -E 'FROM|image:' Dockerfile docker-compose*.yml

# 3. 安全公告 review
# 关注 Go / nginx / MySQL / Redis / RabbitMQ 的 CVE 公告

# 4. 遗留债务清单 review
# 见 §11，决定是否启动某条债务的修复
```

### 8.3 触发型维护

| 触发条件 | 响应 SOP |
| --- | --- |
| Grafana 告警：Consumer backlog 持续 > 100 | 1) 看 RabbitMQ 队列长度 `docker exec cooking-rabbitmq-prod rabbitmqctl list_queues` 2) 看 Consumer 是否健康 3) 考虑临时扩 worker 数 |
| Grafana 告警：MySQL 连接池接近耗尽 | 1) `SHOW PROCESSLIST` 看是否有慢查询 2) `SHOW VARIABLES LIKE 'max_connections'` 3) 临时调大或排查应用泄漏 |
| Grafana 告警：5xx 比例 > 1% | 1) `docker exec cooking-app1-prod grep ERROR /var/log/cooking/app.log \| tail -50` 2) 看 5xx 来源（哪个 endpoint） 3) 走 §9 故障排查 Playbook |
| 真实流量超 80 r/s 持续 1 周 | 1) 启动遗留债务 #4 多机压测 2) 决定是否横向扩 app 实例数 |
| 域名续费提醒（mellowck.com） | 阿里云万网控制台续费；建议直接续 5 年 |
| Let's Encrypt 续期失败 | 1) `ssh root@... 'certbot renew --dry-run'` 2) 看 nginx 80 端口 ACME 路径是否可达 3) 手动续期 |

---

## §9 故障排查 Playbook

### 9.1 HTTPS 证书续期失败

**症状**：cron 邮件提示 certbot renew 失败 / 浏览器警告证书过期

**排查**：
```bash
ssh root@47.238.29.251
certbot certificates                # 看当前证书状态
certbot renew --dry-run             # 模拟续期，看错误
# 常见错误：80 端口 ACME 路径不通 → 检查 nginx 80 server 配置 / 检查 deploy/certbot/webroot 目录权限
```

### 9.2 nginx 配置改了不生效

**症状**：改了 `deploy/nginx/nginx.conf` 并 push，服务器 git pull + `nginx -s reload` 后无效果

**根因**：bind mount 单文件 + git pull → 宿主机文件 inode 变了，容器仍持有旧 inode

**修复**：
```bash
# 必须 force-recreate，reload 不够
docker compose -f docker-compose.prod.yml --env-file .env.prod up -d --force-recreate nginx

# 验证容器内文件是否新版
sha256sum /opt/cooking-platform/deploy/nginx/nginx.conf
docker exec cooking-nginx-prod sha256sum /etc/nginx/nginx.conf
# 两个 sha 必须一致
```

### 9.3 prod 验证脚本失败

**症状**：`make verify-step20` 报 SMS 抓码失败 / 登录 token 拿不到

**排查顺序**：
1. **SMS per-IP 限流**：检查 Redis `sms:ip:*` key 是否累积
   ```bash
   RPW=$(grep '^REDIS_PASSWORD=' .env.prod | cut -d= -f2-)
   docker exec cooking-redis-prod redis-cli -a "$RPW" --no-auth-warning --scan --pattern 'sms:ip:*'
   # 有的话清掉
   ```
2. **日志路径**：prod 日志在 `/var/log/cooking/app.log`（不是 docker logs，因 log.console:false）
3. **抓码 jq 解析**：日志是 zap JSON，字段 `phone` + `code`，按 phone 精确过滤

### 9.4 prod 日志找不到

**症状**：`docker logs cooking-app1-prod` 输出为空

**根因**：prod `log.console: false` 是故意的，stdout 零输出

**正确方式**：
```bash
docker exec cooking-app1-prod tail -f /var/log/cooking/app.log
docker exec cooking-app2-prod tail -f /var/log/cooking/app.log

# 按字段过滤（zap JSON）
docker exec cooking-app1-prod sh -c "grep ERROR /var/log/cooking/app.log | jq ."
```

### 9.5 端到端验证脚本 HEAD vs GET 误判

**症状**：`curl -sI` 返回 404，但浏览器或 `curl -i` 返回 200

**根因**：`-I` / `-sI` 发的是 HEAD 请求，Gin `r.GET("/health", ...)` 不响应 HEAD

**修复**：验证命令统一用 `curl -s -i`（GET 请求 + 显示完整响应头）

---

## §10 红线（永远不做）

1. **不在仓库中 commit `.env.prod` / `.env.prod.local`**：`.gitignore` 已硬化，包括裸名 SSH 密钥兜底规则
2. **不在 prod 容器内修改任何配置文件**：所有配置改动走 git → push → 服务器 pull → recreate
3. **不在 nginx 公网入口暴露 `/metrics`**：监控端点 prod 只走内网 scrape
4. **不为压测临时禁用 nginx 限流**：限流是生产安全网，单机自压测的物理限制不该靠改 prod 来绕
5. **不引入评论 / 私信 / 直播功能**：产品决策，永久排除
6. **不在没有偏离点登记的情况下修改已完成文件**：偏离点是 PRD 演进的唯一权威依据
7. **不删除任何已发布的错误码定义**：错误码是 API 契约的一部分（已发布 API ABI 不可破坏）

---

## §11 遗留债务清单

按优先级排序。启动任一条债务的修复都视为进入 cooking-platform v2。

| # | 债务 | 优先级 | 工作量 | 处理思路 |
| --- | --- | --- | --- | --- |
| 1 | **prod 日志归集**（Loki / Promtail） | P1 | 2-3 天 | 部署 Loki + Promtail 容器；Grafana 加 Loki datasource；prod log 文件挂出到 Loki |
| 2 | **prod 调试工具链** | P2 | 与 #1 合并 | 日志归集到位后自然解决 |
| 3 | **nginx.conf 单文件 → 目录挂载** | P2 | 1 天 | 改为 `./deploy/nginx:/etc/nginx/conf.d/`；不再踩 inode 失效坑 |
| 4 | **多机客户端压测方案** | P3 | 3-5 天 | k6 cloud 或自建 wrk 集群；测真实极限 QPS |
| 5 | **HSTS preload 名单注册** | P3 | 1 小时 | max-age 升到 2 年 + 加 `includeSubDomains; preload`；提交到 hstspreload.org |
| 6 | **关注 Feed 流（F-F02）** | P2（v2 范围） | 5-7 天 | 推 / 拉 / 混合模式选型；引入 Feed 服务 |
| 7 | **视频内容** | P3（v2 范围） | 2-3 周 | 转码服务 + 存储成本评估 |

---

## §12 关键信息备份位置

| 项 | 位置 |
| --- | --- |
| `.env.prod` 真实密钥 | **小离本地** `docs/prod.md`（唯一备份，定期检查完整性） |
| 服务器 SSH 私钥 | `~/.ssh/id_cooking_platform`（本地） |
| 域名注册商 | 阿里云万网（mellowck.com 自动续费已开启） |
| OSS Bucket | `cooking-platform-prod`（香港，公共读 + CORS） |
| 阿里云 RAM 子账号 | `cooking-platform-oss` AK/SK 在 `.env.prod` |
| GitHub 仓库 | `git@github.com:quqxiaoli/cooking-platform.git`（Private） |
| GitHub Deploy Key | 服务器端独立生成（不跨机器搬运） |

---

## §13 联系协议

如 Claude Code 在某次会话中：
1. **需要修改红线列出的事项** → 不做，请小离确认
2. **需要新建 PR** → 不做，由小离在本地 commit + push
3. **遇到本文档未覆盖的场景** → 在响应中明确"本场景超出 CLAUDE.md 覆盖范围，建议先更新本文档再执行"
4. **发现本文档与实际代码不一致** → 报告给小离，等待文档更新

---

> 本文档是 cooking-platform 项目从开发期过渡到维护期的"接班材料"。
> 任何修改本文档的操作都视为项目治理动作，必须在 commit message 中明确说明变更理由。
>
> *—— Claude × 小离，2026 年 5 月 20 日*