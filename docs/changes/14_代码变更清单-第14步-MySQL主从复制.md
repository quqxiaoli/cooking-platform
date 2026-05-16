# 代码变更清单 · 第 14 步 · MySQL 主从复制（GTID + DBResolver 读写分离）

---

## 基础设施层

- `deploy/mysql/slave.cnf`（新增）
  · server-id=2，GTID=ON，log_slave_updates=ON（GTID 复制要求 slave 也写 binlog）。
    read_only / super_read_only **不**在此文件——Docker entrypoint 的临时初始化 mysqld 会加载相同配置，super_read_only 会阻止 root 用户创建。改由 init-slave.sh 在运行时 SET GLOBAL。

- `deploy/mysql/init-master.sql`（新增）
  · 挂载至 master 的 `/docker-entrypoint-initdb.d/01-init-master.sql`，数据卷首次初始化时执行。
    创建 `repl@'%'`（mysql_native_password），GRANT REPLICATION SLAVE。只有这一个权限，最小化暴露面。

- `deploy/mysql/init-slave.sh`（新增）
  · 由 mysql-slave-init 容器执行（`restart: "no"` 一次性 init 模式）。
    流程：mysqldump 全量快照（--single-transaction --set-gtid-purged=ON）→ slave RESET MASTER → 恢复 dump → CHANGE REPLICATION SOURCE TO（SOURCE_AUTO_POSITION=1）→ START REPLICA → SET GLOBAL super_read_only=ON。
    设计要点：dump 方案而非 GTID-skip，原因是 skip 会跳过建表 DDL，导致 slave 无表结构，DML 复制失败（error 1146）。

- `docker-compose.yml`（修改）
  · 追加 `mysql-slave` 服务：MYSQL_ROOT_HOST="%"（允许 init 容器从 Docker 网络连接），端口 3307:3306，挂载 slave.cnf。
    追加 `mysql-slave-init` 服务：`restart: "no"` 一次性，`depends_on` 两个 service_healthy 条件，执行 init-slave.sh。
    追加 `mysql-slave-dev-data` volume。
    master 服务追加挂载 `init-master.sql` 到 `docker-entrypoint-initdb.d/`。

---

## Config 层

- `pkg/config/config.go`（修改）
  · `DatabaseConfig` 追加 `SlavesDSN []string`（mapstructure:"slaves_dsn"）。
    `registerDefaults` 追加 `v.SetDefault("database.slaves_dsn", []string{})`——Viper 需要显式 slice 默认值才能正确解析空 YAML 列表，否则 Unmarshal 为 nil。
    设计要点：空 slice → DBResolver 不注册 → 退化为单机模式，现有代码零感知。

---

## Go 代码层

- `cmd/server/main.go`（修改）
  · `initMySQL` 追加 DBResolver 注册逻辑：`len(cfg.SlavesDSN) > 0` 时构建 replicas dialectors，调用 `db.Use(dbresolver.Register(...).SetConnMax*().SetMaxConns())`。slave 连接池参数与 master 保持一致。
    注释更新：说明 DBResolver 对上层透明，`SELECT` 自动路由到 slave，`INSERT/UPDATE/DELETE` 留在 master。
  · import 追加 `"gorm.io/plugin/dbresolver"`。

- `go.mod / go.sum`（修改）
  · 新增直接依赖 `gorm.io/plugin/dbresolver v1.6.2`。
    顺带升级：`gorm.io/gorm v1.25.10 → v1.26.0`，`gorm.io/driver/mysql v1.5.6 → v1.5.7`。

---

## 配置文件

- `configs/config.yaml`（修改）
  · `database` 段追加 `slaves_dsn: ["root:cooking123@tcp(127.0.0.1:3307)/cooking_platform?..."]`。
    dev 环境 slave 端口 3307（docker-compose host mapping）。

- `configs/config.prod.yaml`（修改）
  · `database` 段追加 `slaves_dsn` 生产模板，DSN 使用 `mysql-slave:3306`（Docker 内部网络名）。
    注释说明可追加多个 slave 条目水平扩展读流量。

---

## 收尾

- `scripts/verify_step14.sh`（新增）
  · §1 编译 / §2 slave 容器健康 / §3 IO+SQL 线程状态（performance_schema 视图）/ §4 GTID 模式 / §5 read_only + server_id / §6 写 master 同步 slave（canary row）/ §7 Go 服务器启动验证 DBResolver。
    设计要点：不用 `SHOW REPLICA STATUS\G`（\G 在非交互 -e 模式失效），改用 performance_schema 精确提取字段；§6 写入后轮询最多 1s 验证 replication lag。

- `Makefile`（修改）
  · .PHONY 列表追加 `verify-step14`，追加 `verify-step14` target。
