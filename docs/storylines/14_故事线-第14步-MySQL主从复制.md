# 故事线 · 第 14 步 · MySQL 主从复制（GTID + DBResolver 读写分离）

---

## Part A · 本步故事线（约 1100 字）

### 1. 起点

Step 13 把 EventBus 切到了 RabbitMQ，生产化方向已经明确。进入 Step 14 时，数据库层还是单节点——所有读写都打到同一个 MySQL 实例，Feed 查询、搜索、点赞状态读取全部竞争同一个连接池。

PRD v2.2 §3 明确要求主从复制（GTID 模式）+ GORM DBResolver 读写分离。但 dev.cnf 早在 Step 1 就已经预置了 `gtid_mode=ON`、`binlog_format=ROW`、`server-id=1`——这是当时留下的伏笔，本步就是摘果子的时候。

### 2. 设计思考

**决策 A：为什么选 GTID 而不是传统 binlog position 复制？**

传统主从需要记录 `(File, Position)` 二元组，换 master 时必须手动重新定位。GTID（Global Transaction ID）给每个事务分配全局唯一 ID，slave 只需告诉 master "我已经执行了哪些 GTID"，master 自动发来缺失的部分。在 Step 15 双实例场景下，如果需要 failover，GTID 切换逻辑比 binlog position 简单得多。`SOURCE_AUTO_POSITION=1` 就是这个模式的关键参数。（PRD v2.1 §3.2 ADR-DB-01）

**决策 B：slave 初始化用 mysqldump 而不是 GTID-skip**

第一次尝试了一个"取巧"方案：读取 master 的 `@@gtid_purged`（历史已清除的事务 ID），在 slave 上 `SET GLOBAL gtid_purged` 跳过这些不存在的 binlog，让 slave 从 master 最早可用的 binlog 开始追。理论上成立，但在实际跑的时候报了 **error 1146：Table 'cooking_platform.schema_migrations' doesn't exist**。

原因一下子就清楚了：master 的 gtid_purged 里包含了建表 DDL（transactions 1-39，golang-migrate 的 schema 迁移），skip 掉这些事务意味着 slave 的数据目录是空的，没有任何表结构。当 master 发来 DML（比如 INSERT INTO schema_migrations）时，slave 上根本没有这张表。

正确的姿势是 `mysqldump --single-transaction --set-gtid-purged=ON`：用快照把 master 当前的完整状态（含 schema + 数据 + GTID 状态）复制到 slave，然后再开启增量复制。这就是生产数据库搭主从的标准操作——"先全量，后增量"。

**决策 C：super_read_only 不进 slave.cnf**

一开始按照 PRD conventions 的描述，把 `read_only=ON` 和 `super_read_only=ON` 直接写进 `slave.cnf`。结果 slave 容器永远报 `ERROR 1290: The MySQL server is running with the --super-read-only option so it cannot execute this statement`，无法连接。

原因在于 MySQL Docker 镜像的 `docker-entrypoint.sh` 有一个初始化阶段：它会在正式启动之前，用相同的 conf.d 配置文件启动一个临时 mysqld（`--skip-networking`）来创建初始用户（root、replication user 等）。`conf.d/` 里的 `super_read_only` 在这个临时 server 里同样生效，导致初始化 SQL 全部失败，root 用户从未被正确创建。

尝试过 `command: ["mysqld", "--super-read-only=1"]`，结果一样：Docker entrypoint 会把这个参数原样传给临时 mysqld。

最终方案：`slave.cnf` 不设 read_only，`init-slave.sh` 在 `START REPLICA` 之后执行 `SET GLOBAL super_read_only=ON`。这样初始化阶段正常，运行时保护也到位。代价是 slave 容器重启后 super_read_only 不自动恢复（是内存级设置），需要重新运行 init 容器——在 dev 环境这是可接受的。

### 3. 实现节奏

开工时先跑了 `go get gorm.io/plugin/dbresolver`，顺带把 gorm 从 v1.25 升到了 v1.26。

然后按层推进：
1. **config.go**：给 `DatabaseConfig` 加 `SlavesDSN []string`，`registerDefaults` 里设空数组默认值（Viper 需要显式默认值才能正确解析空 YAML 列表）。
2. **deploy/mysql/**：写 slave.cnf、init-master.sql、init-slave.sh 三件套。
3. **docker-compose.yml**：加 mysql-slave 服务（MYSQL_ROOT_HOST="%"，允许 init 容器从网络连接）和 mysql-slave-init 一次性容器（`restart: "no"`）。
4. **cmd/server/main.go**：`initMySQL` 里加 DBResolver 注册，SlavesDSN 为空时跳过，现有代码零改动。
5. **config.yaml / config.prod.yaml**：dev 填 127.0.0.1:3307，prod 填 mysql-slave:3306 占位。
6. **verify_step14.sh**：7 段验证。

调试过程中踩了三个坑（已写入关键决策）。最终验证结果全绿，replication lag ≤ 100ms。

### 4. 踩坑记录

**坑 1**：`SHOW REPLICA STATUS\G` 在 mysql CLI 的 `-e` 参数里不生效，`\G` 只在交互式模式下是格式指令，非交互模式下被忽略，输出变成一行乱字符。改用 `performance_schema.replication_connection_status` 视图 + `docker exec` 解决。

**坑 2**：GTID-skip 方案触发 error 1146（见决策 B），切换到 mysqldump 方案后引入了新问题：上一次 init 容器运行结束后 slave 处于 `super_read_only=ON` 状态，下次 init 容器的 mysqldump restore 也会因 super_read_only 而失败（因为 dump 中有 INSERT/CREATE TABLE）。解法：重启 slave 容器清除内存级 super_read_only，再跑 init。

**坑 3**：master 的 `init-master.sql`（创建 replication user）只在数据卷首次初始化时运行。由于 master 容器在 Step 13 后已有数据卷，本步修改 docker-compose.yml 导致容器被 Recreate，但卷未重建，init SQL 没有重新执行。手动 `CREATE USER IF NOT EXISTS 'repl'@'%'` 解决，后续 `docker compose down -v && docker compose up` 会自动运行 init SQL。

### 5. 收获

- 深入理解了 GTID 主从的建立流程：不能只看"开启 GTID"就认为搭好了，全量快照 → 设置 GTID 状态 → 开增量复制 这三步缺一不可。
- 理解了 Docker MySQL 镜像的 "临时 server 初始化" 机制，以及为什么某些看起来是"运行时"的配置实际上影响了"初始化时"的行为。
- DBResolver 的设计验证了接口隔离的价值：所有 Repository、Service 代码都依赖 `*gorm.DB`，DBResolver 在 GORM 内部拦截请求，对上层完全透明。

**PRD v3.0 §3 需更新**：补充 DBResolver 集成方式、SlavesDSN 配置格式、slave init 幂等流程说明；slave.cnf read_only 设计决策纳入 ADR-DB-02。

---

## Part B · 关联知识点清单

**基础概念（语言 / 框架层面）**
- GORM DBResolver 插件：Sources / Replicas / Policy 三要素
- `gorm.DB.Use()` 插件注册机制
- MySQL GTID：全局事务标识符，格式 `server_uuid:transaction_id`

**设计模式与架构思想**
- 读写分离（Read-Write Splitting）：写请求 → primary，读请求 → replica
- 透明代理模式：DBResolver 对 Repository 层透明，无侵入
- 降级设计：SlavesDSN 为空时自动退化为单机，零配置变更

**Go 语言特性与并发模型**
- `gorm.io/plugin/dbresolver` 插件接口（`gorm.Plugin` interface）
- `sql.DB` 连接池参数在 master 和 replicas 的独立配置

**工程实践（测试 / 部署 / 可观测性等）**
- MySQL 主从搭建三步：全量快照 → GTID 状态对齐 → 增量复制
- `mysqldump --single-transaction --set-gtid-purged=ON` 一致性快照
- Docker entrypoint 初始化阶段 vs 运行时阶段的差异
- `performance_schema.replication_connection_status / replication_applier_status` 监控视图
- docker-compose `restart: "no"` 模式实现一次性 init 容器

**数据安全与合规**
- slave `super_read_only=ON` 防止误写（即使 SUPER 用户也不能绕过）
- replication user 最小权限：仅 GRANT REPLICATION SLAVE，不授予其他

**可能被延伸追问的关联领域**
- 半同步复制（semi-sync）vs 异步复制：如何保证数据不丢失
- GTID failover：slave 如何接替成为新 master（GTID 集合取交集）
- DBResolver 与事务的交互：事务内的 SELECT 走哪个连接
- 读复制延迟（replication lag）对业务的影响及应对（read-your-writes 问题）
- MySQL Group Replication vs 传统主从：强一致 vs 最终一致
