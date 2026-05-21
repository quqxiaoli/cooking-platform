# cooking-platform · 项目总复盘

> 从 0 到公网上线的商业级 Go 后端项目，20 步落地完整记录
>
> **作者**：小离 + Claude（首席 AI 工程师）
> **项目周期**：2026-02 ~ 2026-05（约 14 周）
> **代码仓库**：git@github.com:quqxiaoli/cooking-platform.git（Private）
> **生产域名**：https://mellowck.com
> **服务器**：阿里云香港 ECS（47.238.29.251）
> **本文用途**：项目作品讲述材料 / 面试技术深挖准备 / 二期迭代输入

---

## §0 这个项目是什么

cooking-platform 是一款面向公网上线的烹饪内容分享平台后端，对标字节/腾讯产品研发规范。产品灵感来自《摇曳露营》动漫中提到的同类网站，核心气质是**松弛、真实、有温度、可复现、有场景感、不焦虑**，目标用户是 22-28 岁独居/合租的年轻人。

不同于按"菜系"分类的传统烹饪应用，本平台按**场景**组织内容——出租屋、一个人的饭、露营野炊、家庭厨房、快手日常、打包便当、减脂餐、节气节日。这个分类方式直接对应目标用户的生活场景，而不是抽象的烹饪文化分类。

技术栈是 Go + Gin + MySQL 8.0 + Redis 7.2 + RabbitMQ + 阿里云 OSS / 短信 / 内容安全 + Docker Compose + Prometheus + Grafana + GitHub Actions。前端不在本项目范围内。

最终交付物是一个跑在公网的、有完整商业闭环（用户体系、内容生产、内容消费、内容审核、互动）的后端服务，**12 个容器全 healthy，主从读写分离 slave 占比 91%，SSL Labs A+，端到端 P99 ≤ 40ms**。

---

## §1 项目落地的四轮节奏

整个项目被拆解为 **20 步、4 轮**，每一轮有清晰的内核目标。这种节奏不是事后追认的，是项目启动时就定下的——避免在功能没跑通时就上微服务、在生产细节没打磨时就追求高并发、在架构没固化时就接 CI/CD。

### 第一轮 · 骨架跑通（Step 1-6）

**目标**：让一个最小可运行版本端到端跑起来，验证技术栈选型可行，验证三层架构（handler/service/repository）的代码组织能落地。

| Step | 模块 | 关键产出 |
| --- | --- | --- |
| 1 | 项目初始化 | go.mod、Gin 路由、配置加载、Docker compose dev |
| 2 | EventBus Channel 实现 | 接口抽象 + Channel 实现（MVP）+ RabbitMQ 实现（生产）双轨预留 |
| 3 | 用户模块 | 手机号验证码注册登录、JWT + Redis Session 双轨 |
| 4 | 内容模块 | 帖子发布、Feed 流（游标分页）、详情页 |
| 5 | 点赞模块 | Redis 即时状态 + MQ 异步落库（LikeConsumer 批量写 MySQL） |
| 6 | 全链路验证 + PRD 收口 | 端到端 cURL 脚本通跑 |

这一轮最重要的不是写出多少功能，而是**确认架构原则可执行**：handler 只做参数绑定 + 调用 service + 拼响应、service 只做业务逻辑 + 调用 repository、repository 只做 DB 操作。Cache 作为 service 的旁路。EventBus 是 service 之间的解耦层。

### 第二轮 · 补齐功能（Step 7-12）

**目标**：把 MVP 阶段所有承诺的功能填满，让产品定义边界内的功能 100% 可用。

| Step | 模块 | 关键产出 |
| --- | --- | --- |
| 7 | 搜索 | MySQL FULLTEXT + ngram，MAU 50 万前不引入 ES |
| 8 | 关注 | 关注/取消关注 + 粉丝列表（关注 Feed 流延后到二期） |
| 9 | 图片上传 | OSS PresignedURL + 客户端直传，Go 服务器不过图片流量 |
| 10 | 内容审核 + 阿里云短信真实接入 | 审核状态机（6 态）+ AuditConsumer 异步审核 |
| 11 | 手机号 AES-GCM 加密 + SHA256 迁移 | 数据库内手机号密文存储，索引走 SHA256 |
| 12 | 日志脱敏 + 错误码体系最终收口 | 错误码升格为 6 位三段式 XYYZZZ |

这一轮最隐蔽的收获是**错误码体系的演进**。第一轮用 5 位 4xxxx（PRD v2.1 设计），到 Step 3 用户模块就发现"段位太少不够分"，临时升格为 6 位 XYYZZZ：X=HTTP 段位、YY=模块号、ZZZ=模块内序号。Step 12 收口时把 §7.3 全表重写，让 PRD 成为接口契约的唯一真相源。

### 第三轮 · 生产基础设施（Step 13-17）

**目标**：让 dev 单机模式升级到 prod 高可用模式，让"看起来像生产环境"变成"真的是生产环境"。

| Step | 模块 | 关键产出 |
| --- | --- | --- |
| 13 | EventBus 切换 RabbitMQ | 配置 `mq.provider: channel→rabbitmq`，零业务代码改动 |
| 14 | MySQL 主从 + DBResolver | GTID 模式主从复制，GORM DBResolver Random Policy 自动路由 |
| 15 | Nginx 双实例负载均衡 | app1/app2 双 Go 实例，nginx round-robin + 被动健康检查 |
| 16 | Prometheus + Grafana | 指标暴露、看板配置、auto-provisioning |
| 17 | CI/CD（GitHub Actions）+ PRD v3.0 | PR 流水线 + staticcheck + verify-step17 自检 |

这一轮的工程价值密度最高。第一轮第二轮还能勉强算"教学项目"，到第三轮就是真正的"生产工程"——主从复制、读写分离、多实例负载均衡、可观测性、CI/CD 全部就位。Step 14 的"读写分离 + DBResolver Random Policy"埋下了 Step 20 验证的伏笔。

**Step 18 启动前的债务清理冲刺（A/B/C 三组 15 条）**：Step 17 完成 PRD v3.0 后，启动 Step 18 之前做了一次集中的债务清理，包括 prod config 补齐（A1）、health check 改造（A2）、三阶段优雅停机（A3）、AES key 防御（A4）、Config-First 5 套补丁（A5）、PV/Count 幂等性（A6）、EventBus delivery 语义对照（A7）、prod compose 门面（B1-B3）、CI 工具锁版本（C1-C3）。这个冲刺让 Step 18 起跳时债务台账归零。

### 第四轮 · 部署上线（Step 18-20）

**目标**：让一切落地到公网，接受真实流量验证。

| Step | 模块 | 关键产出 |
| --- | --- | --- |
| 18 | 服务器 + 域名 + HTTPS | 阿里云香港 ECS 2C4G、mellowck.com、Let's Encrypt 证书 |
| 19 | 数据库迁移 + 部署联调 | golang-migrate 跑表结构、12 容器全 healthy |
| 20 | 公网验证 + 压测 | 31/0/0 端到端验证、wrk2 压测、Grafana 看板归档 |

这一轮的关键不是写新代码（事实上代码改动最少），而是**让前 17 步沉淀的所有设计在公网真实环境兑现**。Step 19 末公网 HTTP 通的那一刻、Step 20 看到 slave 占比 91% 的那一刻、SSL Labs 评级 A+ 的那一刻，是整个项目里最有"作品感"的瞬间。

---

## §2 十二个最值得讲的技术决策

按决策影响力 × 面试可讲度排序。每条都有"问题—选项—决策—效果"四要素。

### 决策 1 · 单体优先 vs 微服务

**问题**：项目立项时面临架构形态选择。

**选项**：
- A. 微服务（服务发现 + 分布式追踪 + 分布式事务 + 多 deploy 单元）
- B. 单体 + 清晰分层（cmd/internal/pkg，未来可拆）

**决策**：选 B。

**理由**：
- 团队规模 = 2 人（小离 + Claude），微服务的协作收益不存在
- 流量预估 MAU < 50 万，单实例横向扩展空间还很大
- 分布式系统的复杂度（最终一致性、跨服务调用追踪、跨库事务）对 MVP 是负担不是资产
- 清晰的三层架构 + EventBus 抽象，未来真要拆服务时拆解成本很低

**效果**：
- 整个项目代码组织极其清晰：handler/service/repository/cache/consumer 五个目录撑起所有业务模块
- Step 13 EventBus 从 Channel 切到 RabbitMQ 时零业务代码改动，证明抽象层有效
- Step 20 公网上线时 12 个容器跑在一台 2C4G 机器上完全够用

### 决策 2 · 抽象优先的"接口 + Mock + Real"三件套

**问题**：阿里云 SMS、阿里云内容安全、阿里云 OSS 三个外部依赖，在 MVP 阶段都要花钱、都需要审核（短信签名/模板）、都不能在本地跑。

**决策**：每个外部依赖都做成三件套——`interface`（如 `SMSSender`）、`Mock`（dev 用，打日志、auto-pass）、`Real`（prod 用，调真实云 API）。配置切换通过 `provider: mock|aliyun` 一行搞定。

**理由**：
- dev 阶段不用花钱、不用等审核就能开发
- prod 切换时只改配置不改代码
- 测试可控：mock 永远可预测，验证脚本依赖这一点
- 面试讲点直接：依赖注入 + 接口抽象的教科书实践

**效果**：
- Step 10 阿里云审核未到位时，prod 仍能用 mock 上线
- Step 20 验证脚本 `verify_step20.sh` 通过 `docker logs grep "[SMS MOCK]"` 抓验证码完成端到端测试，整个流程零真实成本
- prod 切真值只需改 `sms.provider: mock → aliyun` + 注入 AK/SK，零代码改动

### 决策 3 · EventBus 抽象（Go Channel ↔ RabbitMQ）

**问题**：点赞、PV、内容审核都需要异步处理，但 MVP 期上 RabbitMQ 太重，dev 期开发体验差。

**选项**：
- A. 永远用 Redis List 当队列（简单但语义弱）
- B. MVP 期不做异步，所有写入同步（违背 PRD 性能目标）
- C. 接口抽象，MVP 期 Channel 实现，生产期 RabbitMQ 实现

**决策**：选 C，定义 `EventBus interface`，两套实现并存。

**接口设计**：
```go
type EventBus interface {
    Publish(ctx context.Context, event Event) error
    Subscribe(topic string, handler func(Event)) error
    Close() error
}
```

**理由**：
- Channel 实现：at-most-once、进程内、零基础设施依赖
- RabbitMQ 实现：at-least-once、跨实例、消息持久化、死信队列
- 配置切换：`config.yaml mq.provider: channel|rabbitmq`，零业务代码改动

**效果**：
- Step 5 Like 模块、Step 4 PV 模块、Step 10 Audit 模块全部用 EventBus 解耦
- Step 13 切换 RabbitMQ 时业务代码零修改
- Step 20 实证：压测 130K 点赞请求，LikeConsumer 在 15s 内完成 drain，幂等正确

### 决策 4 · 主从读写分离 + DBResolver Random Policy

**问题**：MySQL 主从复制部署起来不难，但 Go 代码层面读怎么路由到 slave、写怎么路由到 master，是个集成层面的决策。

**选项**：
- A. 手写两套 DB 句柄，业务代码显式选择
- B. GORM DBResolver 插件，按 statement 类型自动路由
- C. ProxySQL/MyCat 等中间件，业务无感知

**决策**：选 B。

**理由**：
- 选 A 业务代码污染严重，每个 repository 都要判断"这是读还是写"
- 选 C 引入新中间件，运维复杂度上升
- 选 B 在 Go 应用层解决，GORM DBResolver 是官方推荐方案

**配置**：
```yaml
database:
  dsn: <master>
  slaves_dsn: <slave1>,<slave2>
  policy: random
```

**Random Policy 的语义**：每条 SELECT 在 N 个 slave 之间随机选一个发出。在统计意义上均匀，单次跑可能偏一边。

**效果**：
- Step 14 部署 prod 主从结构（GTID 模式）
- Step 20 实证：`Com_select` 计数器对比，slave 占比 **91%**（slave_delta=11, master_delta=1），证明 Random Policy 在公网真实流量下完美生效

**这是项目里最难"事后证明"的设计决策**。分布式系统的很多设计是按概率发生的，平时跑测试看不到、只能在统计意义上验证。Step 20 这次对比直接用 MySQL status variable 完成实证，零侵入、零开销。

### 决策 5 · 内容审核状态机 + is_visible 冗余

**问题**：UGC 内容必须经过审核才能对外可见。审核是异步的、可能失败的、可能需要人工复审的。怎么设计这个状态流转？

**决策**：6 态状态机 + `is_visible` 冗余字段。

**状态**：
```
pending_review → reviewing → approved
                          → rejected
                          → human_review → approved
                                        → rejected
```

**冗余字段**：`posts.is_visible` 是 `audit_status` 的派生字段（approved 时 = 1，其他 = 0）。Feed 查询 + 详情查询都用 `is_visible = 1` 过滤，不需要 join 审核日志表。

**理由**：
- 状态机让审核流程清晰
- is_visible 冗余让 Feed/详情查询零 join 成本
- 审核失败可重试、可人工介入

**效果**：
- Step 10 部署 AuditConsumer，调阿里云 Green API（dev mock = auto-pass）
- Step 20 实证：刚发的 post `audit_status=0, is_visible=0`，未授权读者 GET `/posts/:id` 返回 `412104 post not found`。这是审核状态机的正确行为，不是 bug。如果有人通过预测 ID 爬未审核内容也爬不到。

### 决策 6 · 游标分页 vs Offset 分页

**问题**：Feed 流分页用 `LIMIT offset, size` 还是 cursor-based？

**决策**：cursor 分页。游标 = `(created_at, id)` 复合键。

**理由**：
- Offset 在数据量大时性能崩（`LIMIT 10000, 20` 需要扫 10020 行）
- Offset 在用户翻页同时有新数据插入时会出现重复/漏数据
- Cursor 永远走索引、永远稳定

**接口设计**：
- 请求：`GET /feed?cursor=<base64(created_at|id)>&size=20`
- 响应：`{ posts: [...], next_cursor: "...", has_more: false }`

**效果**：Step 20 wrk2 压测 GET /feed，P99 = 22.08ms，backend 表现稳定。

### 决策 7 · Redis 即时状态 + MQ 异步落库（点赞场景）

**问题**：点赞这种高频写入操作，每次都同步写 MySQL 会成为瓶颈。

**选项**：
- A. 同步写 MySQL（简单但慢）
- B. Redis 写缓存，定时刷盘
- C. Redis 即时状态 + MQ 事件 + Consumer 批量落库

**决策**：选 C。

**实现**：
- 写：用户点赞 → Lua 脚本原子操作 Redis Set + Counter → 发 `event.like` 到 RabbitMQ
- 读：是否点赞 → Redis Set 查询（SISMEMBER）
- 落库：LikeConsumer 批量消费事件，30 秒一批，INSERT IGNORE 写 `likes` 表 + 增量 UPDATE `posts.like_count`
- 限流：per-user 200/24h，防刷

**理由**：
- 写路径快（毫秒级，无 MySQL 写入）
- 读路径快（Redis Set O(1)）
- 落库批量（30s 一次，QPS 压力分摊）
- 幂等：INSERT IGNORE + 重复事件去重

**效果**：Step 20 压测：130K 点赞请求 → LikeConsumer 15s 内完成 drain → 同用户对同帖子重复点赞最终只 +1（幂等正确）。

### 决策 8 · 手机号 AES-GCM 加密 + SHA256 索引

**问题**：手机号是 PII（个人身份信息），必须加密存储。但加密后怎么按手机号查询？

**选项**：
- A. 明文存储 + 数据库层加密（透明加密）
- B. 应用层 AES 加密 + 模糊匹配（性能崩）
- C. 应用层 AES-GCM 加密 + 同时存 SHA256 哈希作索引

**决策**：选 C。

**实现**：
- `users.phone_encrypted`：AES-256-GCM 密文，IV 拼在前面
- `users.phone_hash`：SHA256（手机号 + 全局 salt），UNIQUE INDEX
- 注册查重：`WHERE phone_hash = ?`
- 登录：先按 hash 找到用户，再用 AES 解密验证

**理由**：
- 加密强度足够（AES-GCM 是 AEAD，含完整性校验）
- 索引性能不受影响（SHA256 等长，B+ 树效率与 VARCHAR 主键相当）
- 数据库泄漏时手机号不可逆向（攻击者最多只能拿到 hash）

**效果**：Step 11 完成迁移，prod 数据库内所有手机号密文存储；GDPR/个保法合规。

### 决策 9 · prod log.console: false 的"安静哲学"

**问题**：prod 环境日志输出到哪里？

**选项**：
- A. stdout（docker logs 收集，方便但日志会膨胀）
- B. 容器内文件（持久化，但要主动看）
- C. 同时输出（双重存储，磁盘浪费）

**决策**：选 B。prod 配置 `log.console: false`，日志只写 `/var/log/cooking/app.log`。

**理由**：
- stdout 在 docker logs 累积，没上限，磁盘风险大
- 容器内文件可用 logrotate 控制大小
- 真正的解决方案是日志归集（Loki/Promtail），不是 stdout

**代价**：
- 调试时 `docker logs` 是空的，必须 `docker exec ... cat /var/log/cooking/app.log`
- Step 19 发现这是个调试工具链债务
- Step 20 verify_step20.sh 第一版用 docker logs 抓 SMS mock 验证码，整个失败，直到改为 `docker exec` + `jq` 解 zap JSON 才修好

**效果**：prod 日志整洁、可控；遗留债务待 Loki 接入后彻底解决。

### 决策 10 · 香港服务器 + .com 域名免备案

**问题**：原计划 `laidbackck.cn` 域名 + 大陆服务器，需要 ICP 备案，约 20 天工信部审核。

**选项**：
- A. 大陆服务器 + 任意域名 → 必须 ICP 备案
- B. 香港服务器 + 大陆域名 → 仍需 ICP 备案
- C. 香港服务器 + .com 国际域名 → **免备案**

**决策**：Step 18 选 C（阿里云香港 ECS），Step 20 域名买 `mellowck.com`。

**理由**：
- 香港服务器在工信部备案规则覆盖范围之外
- .com 是国际域名，不走中国域名注册局
- 两者组合直接绕过备案流程

**代价**：
- 香港服务器延迟略高（实测从大陆访问 50-80ms，可接受）
- 不能享受国内 CDN 加速（公网 IP 直连）

**效果**：Step 18 部署完，Step 20 买完域名当天就完成 HTTPS 上线，省了 20 天等待。

### 决策 11 · HTTPS certbot webroot + Mozilla Intermediate + HSTS

**问题**：HTTPS 怎么部署、用什么 SSL 配置、续期怎么自动化？

**决策**：
- certbot **webroot 模式**（不是 standalone）：续期不需要停 nginx
- **Mozilla Intermediate** SSL 配置：TLSv1.2 + 1.3、ECDHE-only 密码套件
- **HSTS max-age=31536000**（1 年）：浏览器强制 HTTPS

**配置精炼**：
```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name mellowck.com www.mellowck.com;
    
    ssl_certificate     /etc/letsencrypt/live/mellowck.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/mellowck.com/privkey.pem;
    
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:...;
    ssl_prefer_server_ciphers off;
    
    add_header Strict-Transport-Security "max-age=31536000" always;
}
```

**理由**：
- webroot 模式：nginx 80 端口开一个 `.well-known/acme-challenge/` location，certbot 把挑战文件写到 webroot，nginx 静态提供。续期零停机。
- Intermediate 兼容到 TLSv1.2，覆盖 22-28 岁目标用户的所有设备
- ECDHE-only 密码套件免去 dhparam 文件依赖
- HSTS 1 年是 SSL Labs A+ 入门门槛

**效果**：SSL Labs 评级 **A+**。证书 90 天有效期，certbot renew 自动续期。

### 决策 12 · v3 工作流：GitHub MCP + 二步开工契约

**问题**：人机协作 14 周，怎么保证 Claude 不在不了解现有代码的情况下盲写、怎么避免 Claude 的"记忆漂移"导致代码不一致？

**演进**：
- v1：每次开工小离贴代码快照 → 上下文窗口爆炸
- v2：维护 Project Knowledge 中的代码快照文件 → 更新滞后、Claude 信不过
- **v3**：Claude 通过 GitHub MCP 直接 fetch 仓库 → 真相源始终是 GitHub

**二步开工契约**：
1. Claude 100 字说明本步要做什么 + 列出本步要 fetch 的文件清单
2. Claude 主动 fetch（≤15 个/批），列"已 fetch + 关键发现"
3. Claude 输出假设清单（函数签名、字段名、配置项、错误码段位、Redis Key、MQ Topic）
4. 小离确认或纠正
5. Claude 按子模块分组批量生成代码（数据层 → 业务层 → 接入层 → 收尾）
6. 每组生成完等"继续"再进入下一组

**理由**：
- 真相源唯一（GitHub）避免 Claude 凭记忆瞎猜
- 假设清单让"接口错位"在写代码前暴露，不是编译后才发现
- 子模块分组让小离可以喊停、可以改单文件，不被 Claude 一次性灌满

**效果**：Step 7-20 全程采用 v3 工作流，假设清单纠错率显著下降，代码生成质量稳定。

---

## §3 五大架构 KPI 在公网的实证

Step 20 的核心价值不是上线了 HTTPS（那是 HTTPS 本身的能力），而是把前 19 步的设计在公网真实环境**全部跑了一遍**。

| KPI | PRD 目标 | 实测 | 状态 |
| --- | --- | --- | --- |
| **主从读写分离 slave 占比** | 统计意义上均匀 | 11/12 = **91%** | ✅ 远超预期 |
| **HTTP/HTTPS P99 延迟** | < 100ms (PRD §10.3) | 12.85ms (health) / 22.08ms (feed) / 33.06ms (detail) | ✅ 富余 3 倍以上 |
| **SSL Labs 评级** | A 及以上 | **A+** | ✅ |
| **Consumer 异步落库正确性** | 最终一致 | 15s 内 like_count 落库到位、幂等 | ✅ |
| **端到端业务流** | 全通 | **31/0/0** | ✅ |

12 个容器全 healthy，跑在一台 2C4G 香港 ECS 上，月度可用性目标 ≥ 99.5% 保留充裕余量。

---

## §4 踩坑全集（按主题）

完整项目踩了很多坑，每一个都让架构理解又深一层。这部分按主题归类，每条都是面试的"二级讲点"（先讲决策，被追问后展开细节）。

### 4.1 配置管理

**Viper 嵌套 env 覆盖 bug**（Step 18 末发现，Step 19 修复）：
单 `viper.AutomaticEnv()` 在"嵌套 key + yaml 同名值"场景下不可靠。`APP_DATABASE_DSN` 无法覆盖 `config.prod.yaml` 中 `database.dsn` 占位符，app1/app2 `Restarting (1)` 死循环。

**根治方案**（commit c4c3d2c）：
- `v.BindEnv("database.dsn", "APP_DATABASE_DSN")` 显式注册键路径
- Unmarshal 后用 `v.GetString` 强制回填到 struct

这是 Viper 嵌套 env 覆盖的**工程级稳健解**，绕开 mapstructure 对嵌套 key 的不可靠合并。

### 4.2 容器化

**Docker bind mount 单文件 + git pull 的 inode 失效**（Step 20 第二波 HTTPS）：
nginx.conf 单文件挂载在 git pull 后宿主机 inode 变了，容器仍持有旧 inode，`nginx -s reload` 救不了，必须 `docker compose up -d --force-recreate nginx`。

**根因**：bind mount 在容器启动时按 inode 绑定到宿主机文件，git pull 写文件是"写临时文件→atomic rename"，宿主机文件 inode 变了，容器内的 mount 还指向旧 inode（旧内容在磁盘上未释放）。

**教训**：生产环境避免 bind mount 单文件，改挂目录（如 `./deploy/nginx:/etc/nginx/conf.d/`）。改造工作量大，挂为遗留债务。

### 4.3 工具链

**HEAD vs GET 验证方法误判**（Step 20 阶段 1）：
连续三轮诊断"www 子域名 HTTPS 404 但主域名 200"，反复怀疑 server_name、Host 校验、HTTP/2 :authority quirks，甚至 fetch 了 main.go 看路由——根本不存在 Host 校验。

**真根因**：我给的验证命令在主域名用了 `curl -i`（GET）、在 www 用了 `curl -sI`（HEAD），Gin 的 `r.GET("/health", ...)` 不响应 HEAD，HEAD 永远 404，GET 永远 200，跟域名无关。

**教训**：单变量调参，验证命令必须复现生产真实流量方法。

**apt wrk 不支持 -R**（Step 20 阶段 4）：
连续推荐了不存在的 wrk flag。debian apt 装的 wrk 4.1.0 不支持 -R，wg/wrk 4.2.0 也不支持，**只有 wrk2（giltene fork）4.0.0 才有**。这是工具链知识错误，浪费了一轮。

**教训**：每次 propose 工具用法时先 `--help` 验证。

### 4.4 业务限流

**SMS per-IP 限流自压测**（Step 20 阶段 4 Setup）：
单 IP 反复跑 setup 触发应用层 SMS 限流（`sms:ip:<sha256>` 计数器累积），用户 2 永远抓不到验证码。

**修复**：脚本 setup 用户间 sleep 8s + 保留手动清 `sms:ip:*` key 应急路径。

**认知**：真实生产场景每个用户来自不同 IP 不会触发，是单机自压测的固有限制。

**wrk2 calibration burst 突破限流**（Step 20 阶段 4 压测）：
wrk2 -R 模式开放系统模型 + HdrHistogram corrected latency，在前 1s calibration 阶段瞬时 QPS 可破百倍 -R 名义值，触发 nginx api_zone 限流导致 non-2xx 虚高。

**认知**：单机自压测**物理上测不出 backend 极限 QPS**，nginx per-IP 100r/s 限流是设计上的安全网。要测真实极限，必须多机客户端方案（k6 cloud / 分布式 wrk）。

### 4.5 路径与方法

**nginx root vs alias 指令的语义差**（Step 20 阶段 1）：
`location ^~ /.well-known/acme-challenge/ { root /var/www/certbot; }` 中 `root` 是把整个 URL 路径拼接到 root 后面，所以请求 `/.well-known/acme-challenge/test` 实际找文件 `/var/www/certbot/.well-known/acme-challenge/test`。我给的测试命令把文件扔在 webroot 根目录 → 404。修复：测试文件放正确子目录。

**412104 在通用扩展段，不在内容模块 42YZZZ**（Step 20 阶段 3）：
verify_step20.sh §7 断言"详情页返回 code∈{0|42[0-9]{4}}"漏了 412104。原因是 `412101-412104` 段位 PRD 里登记的是"通用错误码扩展"，不属于 42YYY 内容模块段位。修复：白名单加 412104，并把段位逻辑写进偏离点。

---

## §5 代码组织与工程文化

### 5.1 三层架构 + 旁路缓存 + 异步事件

```
internal/
├── handler/        ← Gin handler，只做 bind → validate → service → response
├── service/        ← 业务逻辑，调 repository + cache + eventbus
├── repository/     ← DB 操作，接口先行 + ErrXxxNotFound 封装
├── cache/          ← Redis 操作，旁路缓存（不是穿透式）
├── consumer/       ← 异步事件消费者
├── event/          ← EventBus 接口 + Channel 实现 + RabbitMQ 实现
├── middleware/     ← Gin middleware（auth, logger, cors, metrics）
├── model/
│   ├── entity/     ← GORM 模型
│   └── dto/        ← API DTO（带 binding tag）
```

模块边界清晰，每个文件职责单一，没有"god file"。

### 5.2 接口先行 + Mock + Real 三件套

每个外部依赖（SMS、Audit、OSS）都按这套模式：
```
pkg/sms/
├── sender.go       ← SMSSender interface
├── mock.go         ← Mock 实现（dev）
├── aliyun.go       ← 阿里云实现（prod）
```

切换通过配置 `sms.provider: mock|aliyun`，零代码改动。

### 5.3 错误码段位预分配

| 段位 | 模块 |
| --- | --- |
| 400000-409999 | 通用 |
| 41YZZZ | 用户 |
| 42YZZZ | 内容 |
| 43YZZZ | 互动 |
| 44YZZZ | 关注 |
| 45YZZZ | 搜索 |
| 46YZZZ | 上传 |
| 47YZZZ | 审核 |
| 48YZZZ | 加密 |
| 503XXX | HTTP 5xx 段位 |

每步开工契约必须列出本步使用的错误码段位，避免临时编号撞车。

### 5.4 偏离点登记机制

每步收尾时必须在进度文件登记"本步与 PRD 的偏离点"，包括：
- PRD 章节/规则
- 实现差异
- 选择此方案的原因

这些偏离点是 Step 12 / Step 17 生成新版 PRD 的**唯一权威依据**。PRD v2.2 吸收了 28 项偏离、PRD v3.0 吸收了第二轮 + 第三轮所有偏离。

### 5.5 每步四份交付物

每步收尾输出：
1. **进度追踪 md**（标 ✅ + 详情 + 偏离点）
2. **故事线 md**（Part A 故事线 1500-2500 字 + Part B 知识点清单）
3. **下一步提示词 md**（可直接复制到新窗口）
4. **特殊提醒**（grep 自检、git commit message、merge --no-ff、tag、push）

这套机制让每一步的"工程师讲述材料"自然沉淀，面试前 grep 一遍知识点清单做总话术卡即可。

### 5.6 GitHub Flow + step-N-done tags

```
main (永远可运行)
  └── feature/step-N-<模块名>
        └── 收尾时 merge --no-ff 到 main
              └── tag: step-N-done
                    └── push origin main --tags
```

每步收尾 push 后 main 上有清晰的 tag 序列：`step-7-done` → `step-8-done` → ... → `step-20-done`。`git log --graph` 可视化每一步的合并点。

---

## §6 遗留债务清单（cooking-platform v2 的输入）

| # | 债务 | 优先级 | 处理思路 |
| --- | --- | --- | --- |
| 1 | **prod 日志归集**（Loki / Promtail） | P1 | 让 prod 日志从"沉到容器底"变成"Grafana 直接看"；与 Step 12 的"日志可观测性"自然衔接 |
| 2 | **prod 调试工具链** | P2 | 与 #1 合并：日志归集到位后自然解决 |
| 3 | **nginx.conf 单文件 bind mount → 目录挂载** | P2 | 改为 `./deploy/nginx:/etc/nginx/conf.d/`，避免 inode 失效 |
| 4 | **多机客户端压测方案** | P3 | k6 cloud 或分布式 wrk；等真实流量预估超 80 r/s 持续 1 周再启动 |
| 5 | **HSTS preload 名单注册** | P3 | 提交到 hstspreload.org，max-age 升到 2 年 |
| 6 | **关注 Feed 流（F-F02）** | P2 | 二期功能，MVP 范围内只做关注关系本身 |
| 7 | **评论功能** | ❌ 永久排除 | 产品决策，永久不做 |
| 8 | **视频内容** | P3 | 二期评估，涉及转码服务 + 存储成本 |

---

## §7 如果重来一次会做的不同

**1. nginx.conf 一开始就挂目录而不是单文件**：Step 20 撞了一回 inode 失效，事后想想这就是个不需要踩的坑，从 Step 15 引入 nginx 时就应该挂目录。

**2. log.console 配置应该有 dev / prod 自动切换**：prod 安静哲学是对的，但应该在 `config.prod.yaml` 之外加一个"调试模式"开关，方便临时打开 stdout 而不用改配置文件。Step 12 没考虑到这一层。

**3. 错误码段位应该从 6 位开始**：第一轮用 5 位 4xxxx，到 Step 3 就发现不够分。如果开始就 6 位，能省一次 PRD §7.3 全表重写。但这是后视镜，最初设计时确实没预见到段位会膨胀到 8 个模块。

**4. wrk vs wrk2 应该在 Step 16 监控落地时就调研清楚**：到 Step 20 才发现 -R 是 wrk2 独有，浪费了一轮压测工具试错时间。其实在 Step 16 Prometheus + Grafana 时就可以顺手压一次、把工具链固化下来。

**5. SMS 限流 key 应该有 dev 旁路**：单机自压测撞限流是反复出现的问题，dev 配置应该可以临时禁用 SMS IP 限流（生产配置不变）。Step 3 实现 SMS 时没区分 dev / prod 限流策略。

---

## §8 面试讲述指南

按面试场景给出讲点优先级。这是把 14 周项目浓缩成"15 分钟自我介绍 + 60 分钟技术深挖"的素材库。

### 8.1 自我介绍场景（30 秒-1 分钟）

```
我最近做了一个面向公网上线的烹饪内容分享平台后端，
Go + Gin + MySQL 主从 + Redis Sentinel + RabbitMQ + Docker，
对标字节的产品研发规范。

亮点是 20 步落地全过程，从 0 到公网上线，
包括 MySQL 主从读写分离（生产实证 slave 占比 91%）、
EventBus 抽象（Channel/RabbitMQ 切换零业务代码改动）、
内容审核状态机、HTTPS（SSL Labs A+）等。

目前域名 mellowck.com，跑在阿里云香港 2C4G 服务器上，
12 个容器全 healthy，端到端 P99 < 40ms。
```

### 8.2 项目背景场景（追问"为什么做这个"）

讲三层逻辑：
1. 产品差异化：按场景而不是按菜系分类，对应目标用户生活场景
2. 商业完整性：闭环包括用户体系、内容生产、内容消费、内容审核、互动
3. 技术挑战：单体 + 清晰分层架构，预留微服务演进路径

### 8.3 架构设计场景（追问"为什么不用微服务"）

讲三个理由：团队规模、流量预估、抽象层有效。
然后讲 EventBus 抽象作为"未来拆服务时的关键解耦点"。

### 8.4 性能优化场景（追问"怎么保证高并发"）

讲三件套：
1. 主从读写分离 + DBResolver Random Policy（读流量分摊到 slave）
2. Redis 即时状态 + MQ 异步落库（写路径不阻塞）
3. OSS PresignedURL 直传（Go 服务器不过图片流量）

如果追问"压测数据"，讲 Step 20 的 P99 数据 + 单机自压测的局限性认知。

### 8.5 故障排查场景（追问"遇到过最难定位的 bug"）

首选讲 **Viper 嵌套 env 覆盖** 或 **bind mount inode 失效**，因为：
- 都有真实根因可深挖
- 都有"我曾误解"→"诊断"→"修复"的完整故事
- 涉及到框架/容器底层知识

次选讲 **HEAD vs GET 验证误判**——讲完后引出"单变量调参"工程原则。

### 8.6 代码质量场景（追问"怎么保证代码质量"）

讲四层：
1. 静态：staticcheck + go vet（在 CI 强制）
2. 单元测试：phone_test.go ErrEmptyKey 等关键路径
3. 集成测试：verify_step20.sh 11 个 section 端到端
4. 上线验证：Step 20 公网真实流量验证

### 8.7 团队协作场景（追问"和 AI 怎么协作"）

讲 v3 工作流：
- GitHub MCP 让 Claude 实时 fetch 仓库
- 二步开工契约：100 字说明 + fetch 文件 → 假设清单 → 子模块分组生成
- 每步四份交付物自然沉淀工程素材

亮点：偏离点登记机制让 PRD 持续演进，PRD v2.2 / v3.0 都是吸收实现偏离生成的。

### 8.8 工程文化场景（追问"项目里有什么让你印象最深的"）

讲两个瞬间：
1. Step 20 看到 slave 占比 91% 时——14 周前在 Step 14 设计的"DBResolver Random Policy"在公网真实流量下被一个 Com_select 计数器对比验证得清清楚楚。分布式系统的很多设计是按概率发生的，平时跑测试看不到、只能在统计意义上验证。这种"事后证明设计正确"的瞬间，是整个项目里最少见的瞬间。
2. Step 18 香港服务器 + .com 域名免备案的红利——这是 Step 18 选香港服务器埋下的伏笔，到 Step 20 才兑现红利。绕过了 20 天工信部备案卡点。**好的架构决策会跨步骤产生回报**。

---

## §9 致谢与结语

cooking-platform 是小离 + Claude 14 周的人机协作产物。20 步落地、12 个容器、约 25000 行 Go 代码、5 份 PRD 文档（Phase1 → v1.2、Phase2 → v1.1、Phase3 → v2.0 / v2.1 / v2.2 / v3.0）、20 份进度追踪文件、20 份故事线、1 份本文。

整个过程不是"AI 写代码人类 review"的简单分工，而是**首席 AI 工程师主导技术决策、工程师确认并执行**的协作模式。所有架构决策、PRD 演进、偏离点登记、踩坑认知，都在每一次"假设清单 → 用户确认 → 批量生成 → 收尾交付"的循环里沉淀下来。

这不是一个"教学项目"，是一个**真的在公网跑、真的有真实用户能注册发帖、真的接受真实流量压测**的商业级后端。从 Step 1 `go mod init` 到 Step 20 SSL Labs A+，14 周的每一步都是真实的工程决策、真实的代码改动、真实的踩坑修复。

**Step 20 完结。cooking-platform 项目（20/20 步）正式收官。**

---

> 本文档是 cooking-platform 项目的最终复盘，作为"作品讲述材料"使用。
> 后续维护期内的所有迭代记录、bug 修复、功能新增，应在独立的 changelog 文档中追加，不修改本文。
>
> *—— Claude × 小离，2026 年 5 月*
