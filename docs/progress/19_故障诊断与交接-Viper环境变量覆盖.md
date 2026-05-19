# Step 19 故障诊断与交接 · Viper 环境变量覆盖问题

> 本文件是 Step 19 阻塞问题的完整交接。新窗口接手时，**先读本文件**，
> 不要重复已做过的诊断。所有"已确证事实"是铁的，不需再验证。

---

## 一、一句话问题陈述

基础设施 4 容器（mysql / mysql-slave / redis / rabbitmq）全部 healthy，
但 **app1 / app2 持续 Restarting，无法启动**。根因隔离到：
**`APP_DATABASE_DSN` 环境变量无法覆盖 `config.prod.yaml` 里
`database.dsn` 的占位符值**，导致 app 拿着无效 DSN（`<DB_USER>` 字面量）
连库失败。

---

## 二、环境与坐标（新窗口需要的上下文）

- 服务器：阿里云香港 ECS，`ssh root@47.238.29.251`，密码用户自存本地
- 代码目录：`/opt/cooking-platform`，git commit `065ca56`，**已撤回所有改动，working tree clean**
- compose 文件：`docker-compose.prod.yml`，env 文件：`.env.prod`（gitignore，已正确填写并校验）
- docker 网络名：`cooking-platform_cooking-prod-net`
- mysql 容器：`cooking-mysql-prod`，容器 IP `172.18.0.4`，**无对外端口暴露**
- DB 业务用户：`cooking`@`%`，密码 = `.env.prod` 中 `APP_DATABASE_DSN` 里 `cooking:` 后的 hex 串（用户自存本地）
- 数据库 `cooking_platform` 已建，6 迁移已 applied，7 表齐全（version=6 dirty=0）
- 启动正确姿势：`docker compose -f docker-compose.prod.yml --env-file .env.prod ...`

---

## 三、已确证事实清单（铁证，勿重复验证）

1. **二进制健康**：不带 `CONFIG_PATH` 直接跑 `/app/server`，app **完整启动并打日志**
   （`mode:debug`、prometheus initialised），最后崩在 `dial tcp 127.0.0.1:3306
   connection refused`——这是读了 dev `config.yaml`（DB 指 localhost）。
   → 证明：二进制能跑、日志系统正常、Go 编译产物正确（ELF amd64，30MB）。

2. **`config.prod.yaml` 是合法 YAML**：`python3 -c "yaml.safe_load(...)"` 通过。
   容器内副本与宿主机 md5 一致（`3845eb84...`），160 行，未被改坏。

3. **build 成功**：`docker build --target builder` 全 CACHED，`go build` 早已通过，
   无编译错误。

4. **带 `CONFIG_PATH=configs/config.prod.yaml` → 零输出 exit 1**：
   - 落文件再 cat：`o.log` / `err.log` 全空
   - 加 `-t`（TTY）：仍零输出 → **排除 stderr 缓冲被 os.Exit 吞掉的理论**
   - main.go 第 74-79 行错误处理是健壮的：
     `cfg,err:=config.Load(); if err!=nil { fmt.Fprintf(os.Stderr,"FATAL: load config: %v",err); os.Exit(1) }`
     但这行 stderr 输出**从未出现** → 程序疑似在 `config.Load()` 内部就退出，未返回 main。

5. **dev 路径正常 / prod 路径零输出**：唯一变量就是 `CONFIG_PATH` 是否指向 prod。

6. **最早期（原始代码 + 原始配置，未注入 BindEnv 时）日志是有输出的**：
   document index 4 显示 `Access denied for user 'cooking'` →
   `Access denied for user '<DB_USER>'`。**"零输出 exit 1" 是在 python 注入
   BindEnv 之后才出现的现象**。这是关键时间线证据。

---

## 四、根因分析（两层）

### 第 1 层 · 配置注入失效（已定位，确定）

`config.prod.yaml`：
```yaml
database:
  dsn: "<DB_USER>:<DB_PASSWORD>@tcp(mysql-master:3306)/cooking_platform?..."
  slaves_dsn:
    - "<DB_USER>:<DB_PASSWORD>@tcp(mysql-slave:3306)/cooking_platform?..."
```
两个问题叠加：
- a) `<DB_USER>` / `<DB_PASSWORD>` 是从未替换的占位符
- b) host `mysql-master` 不存在（compose 里 service 名是 `mysql` / `mysql-slave`）

`config.go` 用 Viper：`SetEnvPrefix("APP")` + `SetEnvKeyReplacer(".","_")` +
`AutomaticEnv()`。`docker-compose.prod.yml` 的 app1 environment 段**确实注入了**
`APP_DATABASE_DSN`（已核对，env 传进容器了）。

**但 `APP_DATABASE_DSN` 没能覆盖 `database.dsn`**。这是 Viper 的已知行为：
`AutomaticEnv()` + `Unmarshal()` 对**嵌套 key**的 env 覆盖不可靠（mapstructure
反序列化时看不到仅存在于 env 的嵌套值，尤其当 yaml 里该 key 已有非空字符串时）。
对比：`mq.url` 能被 `APP_MQ_URL` 覆盖成功（现象证实），`database.dsn` 不行——
两者差异的精确机制尚未 100% 锁定，但**结论确定：`database.dsn` 需要显式
`v.BindEnv` 才能可靠从 env 注入**。app 代码 `gorm.Open(mysql.Open(cfg.DSN))`
直接用 `cfg.Database.DSN`，覆盖失败就用 yaml 的 `<DB_USER>` 字面量 → 连库失败。

### 第 2 层 · 修复尝试引入的新问题（未解决，已回退）

本窗口尝试的修复（已全部 git checkout 撤回）：
- python 注入 12 行 `v.BindEnv(...)` 到 config.go（AutomaticEnv 之后）
- python 改 config.prod.yaml：`dsn: ""`、`slaves_dsn: []`、sms/audit provider→mock

注入后现象变为 **"零输出 exit 1"**（比原来 `Access denied` 更难定位）。
这个零输出**没有被解释清楚**。可能原因（未验证，留给下个窗口判断）：
- BindEnv 注入代码有运行时副作用，使 `config.Load()` 内部异常退出
- 或 `dsn:""` + validate `if cfg.Database.DSN=="" { return err }` + env 覆盖仍未生效，
  组合出某条没有输出的退出路径
- python 字符串替换可能引入了不可见字符（tab/空格混用），编译过但行为异常

**关键判断**：原始代码错误处理正常（事实 6 证明有输出），零输出是注入引入的。
所以**正确修复不应继续在被污染状态上加诊断，而应回干净基线重做**。

---

## 五、当前状态快照

- `git status` = working tree clean，代码 = 原始 commit `065ca56`
- `config.go`：无 BindEnv（原始）
- `config.prod.yaml`：dsn 回到 `<DB_USER>...` 占位符；sms/audit/oss provider 全 = aliyun（原始）
- `.env.prod`：未受影响，16 项已正确填写
- Docker 镜像 `cooking-platform-app1`：**仍是含已撤回代码的旧 build**，需重新 build
- 残留测试镜像：`cooking-build-test`
- DB 层（用户/库/迁移/表）：**已就绪，无需重做**

---

## 六、下个窗口的恢复计划（建议路径）

### 阶段 1 · 干净基线复现（确认原始代码的真实行为）

目的：在干净代码上重新观测，证明"原始代码会正常报错"，把第 2 层噪音清掉。

```bash
cd /opt/cooking-platform && git status   # 确认 clean
# 重新 build 干净镜像
docker compose -f docker-compose.prod.yml --env-file .env.prod build --no-cache app1
# 干净观测：原始代码 + 原始 prod 配置 + 正确 CONFIG_PATH + 正确网络
docker run --rm --env-file .env.prod \
  -e CONFIG_PATH=configs/config.prod.yaml \
  --network cooking-platform_cooking-prod-net \
  --entrypoint sh cooking-platform-app1 \
  -c '/app/server > /tmp/o.log 2>&1; echo "EXIT=$?"; echo ===LOG===; cat /tmp/o.log'
```
预期：**有输出**，应为 `Access denied for user '<DB_USER>'`（DSN 占位符未被
env 覆盖，但 host `mysql-master` 解析或认证失败，总之有日志）。
- 若有输出 → 证明原始代码错误处理正常，零输出是注入 bug，进阶段 2
- 若仍零输出 → 原始代码本身就零输出，需 strace 级深挖（备选，见阶段 4）

### 阶段 2 · 正确方式修复 Viper env 覆盖

根因是 `database.dsn` 需显式 `BindEnv`。这个方向是对的，错在"用 python
盲注 + 同时改 yaml dsn 为空"引入了不确定性。建议**用 Claude Code 在仓库里改**
（而非 project 窗口 python 替换），改动最小化、单一变更、能本地 `go build` 验证：

- 在 `pkg/config/config.go` 的 `v.AutomaticEnv()` 之后，**只加 `database.dsn`
  和 `database.slaves_dsn` 两个 BindEnv**（不要一次绑 12 个，最小变更）：
  ```go
  _ = v.BindEnv("database.dsn", "APP_DATABASE_DSN")
  ```
  slaves_dsn 单主模式可不绑（保持空）。
- `config.prod.yaml` 第 14 行 dsn：**不要改成空字符串**（空会触发 validate
  `database.dsn is required` 且和 env 覆盖时序耦合）。改成把 host 修对、
  保留占位让 BindEnv 覆盖，或保持原样让 BindEnv 的 env 值整体覆盖——
  二选一，单独验证哪个干净。
- 每改一处，本地 `go build ./...` + `go vet`，再 build 镜像，再观测。
  一次只变一个变量。

### 阶段 3 · 重新落实「决定 1」

git 回退把 sms/audit provider 改回了 aliyun。需重新改 prod 为 mock
（否则 release 模式 validate 会因 SMS/audit AK 空而拒启）。这次用 Claude Code
改、commit、push，不用 project 窗口 python。

### 阶段 4 · 备选深挖手段（仅当阶段 1 仍零输出）

- `strace -f -e trace=write,openat,exit_group ./server`（需 `--cap-add SYS_PTRACE`
  + 容器内 `apk add strace`），看进程 exit 前最后的系统调用、读了哪个 yaml、
  有无 write 尝试。系统调用级追踪，程序逻辑骗不了它。

### 阶段 5 · 修复成功后

- app1/app2 → healthy，nginx 自动起
- `curl localhost/health` → `curl 47.238.29.251/health`（Mac 上）公网验证
- 进 Step 19 正式收尾 + Step 20

---

## 七、给新窗口的工作纪律提醒

1. **先读"已确证事实清单"，不要重复验证**（二进制 OK、YAML OK、build OK 已是铁证）
2. **一次只改一个变量**，每次改完立即观测，不要叠多个改动
3. **优先用 Claude Code 在仓库内改 + 本地 go build 验证**，避免 project
   窗口里 python 盲改（本窗口的教训：python 替换无法本地编译验证，引入了
   难定位的运行时问题）
4. 排查输出问题时，最可靠的是「容器内 `> /tmp/x.log 2>&1` 落文件再 cat」
   + 必带 `CONFIG_PATH` + 必带 `--network`，三者缺一会得到误导性结果
   （本窗口踩过：漏 CONFIG_PATH → 误读 dev 配置；漏 network → 误报 no such host）
5. DB 层已完全就绪，**不要重跑迁移、不要重建用户**

---

## 八、本窗口踩坑记录（面试讲述材料 · 真实发生）

- **域名 .cn vs .com / 备案**：香港服务器不触发备案，.cn 信息模板需注册局审核
- **重置 ECS 密码失败**：默认用户 `cooking` 的 PAM 异常，改重置 root 解决
- **SSH 空闲断连**：配 `ServerAliveInterval` 保活
- **OSS 新建强制"阻止公共访问"**：需创建后单独关闭再改公共读
- **base64 密码进 DSN/MQ URL 的转义坑**：预先用 `openssl rand -hex` 生成 URL 安全密码规避
- **核心坑 · Viper AutomaticEnv 对嵌套 key 覆盖不可靠**：`APP_DATABASE_DSN`
  覆盖不了 `database.dsn`，需显式 `BindEnv`。这是本步最有价值的工程认知，
  也是一个高频可追问的面试点（Viper 配置优先级、12-factor 配置注入、
  mapstructure 与 env 的交互）
- **诊断方法论教训**：在被污染状态上反复叠诊断命令是反模式；正确做法是
  回 git 干净基线、对照式定位、一次一变量
