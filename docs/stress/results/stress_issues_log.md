# 全面压测 — 压测工具 / 流程问题记录

> **创建日期**：2026-06-05
> **范围**：压测**脚本、流程、工具配置**自身的 bug 或可用性问题（不是业务代码问题）。
> **维护规则**：发现一条记一条；非阻塞型在 Phase 9 后统一修；阻塞型当场打 workaround，并在条目里记下回滚位置。
> **配套文件**：业务代码 / 配置 / 架构问题记到 `code_issues_log.md`，不混到本文件里。

---

## #S1 stress_test.sh 的 env vars 在某些调用方式下不生效

- **发现于**：Phase 1 冒烟首次执行
- **症状**：用户敲
  ```
  WRK_RATE=10 WRK_DURATION=60s WRK_THREADS=1 WRK_CONNS=10 bash scripts/stress_test.sh 2>&1 | tee ...
  ```
  但 wrk 输出显示 `Running 30s test`，`2 threads and 50 connections` —— 全是脚本默认值
- **根因猜测**：未完全确认。可能是 `source .env.prod` 失败后用户重试时漏了 env 前缀；也可能是某个 shell wrapper 把 env 吃了
- **影响**：实际跑出来是 80 RPS / 30s，不是预期 10 RPS / 60s。叠加 #C1 后表象更乱
- **当前 workaround**：用 `export` 而非命令前缀
  ```bash
  export WRK_RATE=10 WRK_DURATION=60s WRK_THREADS=1 WRK_CONNS=10
  bash scripts/stress_test.sh
  ```
- **建议修复**：脚本 §1 启动时打印实际 `WRK_RATE / WRK_DURATION / WRK_THREADS / WRK_CONNS` 值，让漏传立即可见

---

## #S2 wrk_get_search.lua 关键字硬编码 `verify` [已修 P2 @ Phase 2 前]

- **发现于**：阅读 lua 时（Phase 1 v3 实测确诊为非 2xx 主因之一）
- **症状**：search lua 写死 `wrk.path = "/api/v1/search?q=verify&size=20"`
- **风险**：seed_stress_data.sh 用 5 个关键字（verify/stress/test/feed/搜索），verify 只覆盖 1/5 = 20% 帖子；且单关键字反复打容易触发应用层限流（详见 `code_issues_log.md` #C2）
- **建议修复**（小改）：lua 从 pool TSV 或一个固定关键字数组里随机选

---

## #S3 stress_test.sh 第 19 行注释 `source .env.prod && bash ...` 不能执行

- **发现于**：用户首次跑 seed 时
- **症状**：`.env.prod` 含 `APP_DATABASE_DSN=cooking:xxx@tcp(mysql:3306)/...` 这种带未转义括号的值，bash `source` 会触发 syntax error
- **建议修复**：脚本注释里的用法改成
  ```
  export MYSQL_ROOT_PW=$(grep '^MYSQL_ROOT_PASSWORD=' .env.prod | cut -d= -f2-)
  bash scripts/stress/seed_stress_data.sh
  ```
  或者脚本自己 grep `.env.prod` 取需要的变量
- **当前 workaround**：CLAUDE.md §7.3 已给出 grep 抽变量的模式，告知用户即可

---

## #S4 4 个新 wrk lua 脚本（+ k6_mixed.js）未 commit/push，服务器跑不到 → 100% 非 2xx 表象

- **发现于**：Phase 1 v2（2026-06-05 11:20~11:40）
- **症状**：stress_test.sh §3 新增的 4 个场景 `delete_like / scene_feed / user_posts / follow` 全部 100% 非 2xx
- **排查过程**：
  - 怀疑 lua 模式有问题 → 4 个最小隔离测试全 200 OK
  - 怀疑 init() 读 TSV 失败 → 测试 E 全 200 OK
  - 服务器直跑 `wrk -s scripts/stress/wrk_get_user_posts.lua` → `cannot open ... No such file or directory`
- **根因**：4 个新 lua + k6_mixed.js 在本地是 untracked，从未 commit/push。wrk 报 cannot open 后 silent-fallback 跑默认 GET /，所以 100% 非 2xx
- **修复**：commit `6894236`（已 push），服务器拉取后 Phase 1 v3 4 个场景 0 non-2xx
- **教训**：stress_test.sh §3 应在 `if [ -f ... ]` 守卫里加 explicit 报错（找不到时停止 + 提示，而不是让 wrk silent-fallback）

---

## #S5 stress_test.sh 汇总表只统计 5 个旧场景，漏掉 4 个新场景 [已修 P3 @ Phase 2 前]

- **发现于**：Phase 1 v3（2026-06-05 11:55）
- **症状**：§3 9 个场景全部跑完，结果文件全部生成，但 §6 汇总表只 print `health / feed / detail / search / like` 5 行，新增的 `delete_like / scene_feed / user_posts / follow` 不出现
- **根因**：stress_test.sh 末尾的汇总循环用了硬编码场景列表，没有跟着 §3 扩展
- **影响**：Phase 1 v3 4 个新场景的 QPS / 延迟 / 错误率得手动 grep
- **建议修复**：汇总循环改为读 `$RESULTS_DIR` 下所有 `.txt` 文件，或者维护一个集中场景数组在 §3 §6 共用
- **当前 workaround**：手动 `cd /tmp/stress_results_step20 && grep -E "Requests/sec|Non-2xx|50\.000%|99\.000%" <场景>.txt`

---

## #S6 stress_test.sh §2 setup 创建的 post 卡 audit_status=0 → detail/like 场景必败

- **发现于**：Phase 1 v3（2026-06-05 11:55）
- **症状**：
  - Phase 1 v3 detail 场景 591/591 = 100% non-2xx，like 场景 329/601 = 55% non-2xx
  - DB 查 `SELECT audit_status, is_visible FROM posts WHERE id=63` → `0 0`
- **关系**：表象是脚本问题（detail / like 用不了 setup post），根因是 `code_issues_log.md` #C1（audit consumer slave lag）
- **当前 workaround**（Phase 9 一并回滚）：
  - 方案 A：每次跑 stress_test.sh 后手动
    ```sql
    UPDATE cooking_platform.posts SET audit_status=1, is_visible=1
    WHERE audit_status=0 AND title LIKE 'stress %' OR id=<setup_post_id>;
    ```
  - 方案 B：在 stress_test.sh §2 创建 post 后立刻加一条 force-visible 的 SQL（带 `[全面压测临时变更]` tag）
- **影响**：每次重跑 stress_test.sh 都要清一次。`code_issues_log.md` #C1 修好之前持续存在

---

## #S7 wrk_post_like.lua 单用户 + 单帖反复点赞 → 触发幂等去重 → 60%+ non-2xx [已修 P1 @ Phase 2 前]

- **发现于**：Phase 1 v4（2026-06-05 12:10）
- **症状**：detail/like 修好后 like 仍 368/601 = 61% non-2xx。post 已 force-visible，DB 不再 404
- **根因**：lua 写死单 user (TOKEN_2) + 单 post (POST_ID)，10 RPS × 60s 全部针对同一 (user, post) 对。like 业务在 Redis 上有幂等去重 → 第一次 2xx，后续返回 4xx
- **§4 异步落库判断也被误导**：报告 "未观察到点赞落库" 是因为大量请求被幂等拦截，没真到 consumer
- **影响**：like 的 QPS / 延迟数据其实只代表 Redis 去重命中的 hot path，不是真实点赞写路径
- **建议修复**（小改）：lua 改为
  - 从 `/tmp/stress_pool_users.tsv` 随机选 token
  - 从 `/tmp/stress_pool_posts.tsv` 随机选 post_id
  - 这样每次组合不同，大部分能走到真实写路径
- **现状**：本轮先记录，Phase 2 前修一下（否则 Phase 2-7 like 数据持续失真）

---

## #S8 Phase 0 §4.1 漏放 nginx `limit_conn`（仅放了 `limit_req`）→ Phase 2 r30 大量 429 [已修]

- **发现于**：Phase 2 r30（2026-06-05 11:28），结果 feed/detail/follow/user_posts 等 5 个场景 15-17% non-2xx
- **症状**：nginx access log 出现大量 `upstream=-` 的 429 —— 说明 nginx 直接拒绝没转发到 backend
- **排查**：`docker exec cooking-nginx-prod nginx -T | grep limit_` 发现还有
  ```
  limit_conn_zone $binary_remote_addr zone=conn_zone:10m;
  location / { limit_req zone=api_zone burst=50 nodelay; limit_conn conn_zone 20; }
  ```
  `limit_conn 20` —— 单 IP 并发连接最多 20。Phase 1 v4 WRK_CONNS=10 没事；Phase 2 WRK_CONNS=60 大量请求被截断
- **根因**：全面压测方案 §4.1 只列了 `limit_req_zone` 改 rate，**没列 `limit_conn`**。Phase 0 §4.1 照方案执行就漏了
- **修复**：`deploy/nginx/nginx.conf` location / 的 `limit_conn conn_zone 20` → `10000`，标 `[全面压测临时变更]`；回滚清单 §1 行 3a 已登记
- **教训**：方案里写「关闭限流」时要列全 nginx 所有限流指令（limit_req / limit_conn / 后续可能有的 limit_rate），不能只关一种

---

## #S9 JWT 2h 过期 → 全量压测期内 token 失效，所有 auth endpoint 100% 401

- **发现于**：Phase 2 r60（2026-06-05 20:17 UTC+8），like/follow/delete_like 三场景 100% non-2xx
- **症状**：
  - r60 like 3600/3600 non2xx；同时 follow / delete_like 3600/3600 non2xx
  - 单独 curl + 旧 token 直接打 → `{"code":410xxx,"msg":"...token expired"}`
  - JWT decode：`exp=1780660934` 对应 20:02:14；测试时已 20:17 → 已过期 15 分钟
- **根因**：
  - seed_stress_data.sh 中 `LOGIN_RESP=$(curl ... /auth/login)` 取得的 JWT TTL = 2 小时（业务侧默认）
  - 全面压测 Phase 0 seed → Phase 1~2 全跑完 ≥ 2h，pool TSV 里的 token 集体过期
  - 所有 auth 场景同时 100% 401，外观像 #C3 hard limit，实际是 token 失效
- **影响**：r60 的 like/follow/delete_like 三场景数据完全失真；首次发现时差点误归因为业务层限流
- **当前 workaround**（**在本地 `/tmp/refresh_pool_tokens.sh`**）：
  - 写一个 refresh 脚本：遍历 phone 池 → send-code → fetch_sms_code → login → 重写 `/tmp/stress_pool_users.tsv`
  - 首次跑成功率 63/100（37 个抓 SMS 失败，原因：log flush race，sleep 0.15 太紧）
  - 63 fresh token 已足够 r60/r100，可继续
- **建议修复**（v2）：
  - seed_stress_data.sh 末尾打印 token TTL 提示；或 stress_test.sh §1 启动时校验 pool token 未过期（JWT decode `exp` 与 now 比较），过期则提示先跑 refresh
  - 长稳测试（Phase 5 30 min × 多档）肯定会过期 → refresh 脚本应当固化为 `seed_stress_data.sh --refresh-tokens`

---

## #S11 seed_stress_data.sh --cleanup 引用不存在的 phone 列 → 静默 0 删除 [发现于 Phase 9 回滚]

- **发现于**：Phase 9 回滚 Step D（2026-06-05 22:53）
- **症状**：`bash scripts/stress/seed_stress_data.sh --cleanup` 输出 "已清掉 139* 用户 + 其名下帖子"，但 DB 中 users/posts 数量未变
- **根因**：
  - cleanup 子命令第 84/88 行用 `WHERE phone LIKE '139%'`
  - 但 users 表自 Step 11 字段级加密改造后，phone 列拆成 `phone_hash`（不可前缀过滤）+ `phone_encrypted`（不可前缀过滤）
  - 引用不存在列 `phone` 应当报 `Unknown column 'phone' in 'where clause'`
  - 但 `mysql_q()` 把 `2>/dev/null` 写死，**所有错误被吞**
- **影响**：自 Step 11 加密改造起，所有 cleanup 都是 noop，但因为 DB 是干净的没人发现；本次回滚才碰到 119 条堆积数据
- **当前 workaround**：手工 cascade DELETE（详见极限报告 §6 Step D）
  ```sql
  DELETE l FROM likes l JOIN users u ON l.user_id=u.id WHERE DATE(u.created_at)='2026-06-05';
  DELETE l FROM likes l JOIN posts p ON l.post_id=p.id JOIN users u ON p.user_id=u.id WHERE DATE(u.created_at)='2026-06-05';
  DELETE f FROM follows f JOIN users u ON f.follower_id=u.id WHERE DATE(u.created_at)='2026-06-05';
  DELETE f FROM follows f JOIN users u ON f.following_id=u.id WHERE DATE(u.created_at)='2026-06-05';
  DELETE ps FROM post_steps ps JOIN posts p ON ps.post_id=p.id JOIN users u ON p.user_id=u.id WHERE DATE(u.created_at)='2026-06-05';
  DELETE p FROM posts p JOIN users u ON p.user_id=u.id WHERE DATE(u.created_at)='2026-06-05';
  DELETE FROM users WHERE DATE(created_at)='2026-06-05';
  ```
- **建议修复**：
  - cleanup 改为按 `DATE(created_at) = $RUN_DATE` AND `nickname LIKE '厨友%'` 双条件过滤（safer）；
  - `mysql_q()` 至少捕获 stderr 中的 "ERROR" 行打到 stdout，不要 silent；
  - 或者 seed 阶段就把生成的 user_id 写入 USERS_TSV，cleanup 直接 `IN (...id list)`。

---

## #S10 wrk_get_search.lua 关键字池含中文 "搜索" → wrk 解析 URL 失败 → search 100% non-2xx

- **发现于**：Phase 2 r60 重跑前（2026-06-05 20:15 UTC+8）
- **症状**：wrk 输出仅一行 `invalid URL at 1:22`，stress_results/search.txt 没有 Requests/sec
- **根因**：上一轮 #S2 修复时随手把 seed 用的 5 个关键字直接搬过来，其中 "搜索" 是 UTF-8 多字节。wrk 的 HTTP/1.1 request line URI parser（chunked from `parse_url`）拒非 ASCII，整次运行直接挂 — 不是随机命中 1/5 的事
- **影响**：r60 search 数据缺失；如果不修，Phase 3-7 search 全跑空
- **修复**：把 "搜索" 替换为 ASCII "post"（与 seed 数据里同样存在的关键字），见 commit log；本地已 `M`，待 push（详见 §S 末尾 push 说明）
- **教训**：lua 里写关键字池要 ASCII-only，或者用 `wrk.format` + URL encode 处理多字节

---

*—— 自动维护，发现新压测脚本/流程问题追加在末尾*
