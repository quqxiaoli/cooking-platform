-- migrations/000003_create_likes_table.up.sql
--
-- 点赞关系表（likes）：MVP 互动模块的核心实体。
--
-- 设计要点（与 PRD-Phase3 v2.1 §5.2 / §4.9 对齐）：
--
--   1. 无 deleted_at 列（★ 偏离一般"软删除"惯例，刻意为之）
--      点赞场景下"取消点赞 = 物理删除该行"是更合适的语义：
--        a. 业务上没有"已取消但要审计"的需求，删除即遗忘。
--        b. 软删除会破坏 uk_user_post 唯一约束的语义——同一 (user_id, post_id)
--           被点过又取消后，再次点赞会因为残留的 soft-deleted 行触发唯一索引冲突。
--           除非把 deleted_at 也加进唯一索引（变成 uk_user_post_deleted），
--           而那会让索引体积、维护成本、查询计划全部劣化。
--      其他场景（user/post）保留 deleted_at 是因为它们承载内容/账户审计。
--
--   2. uk_user_post 唯一索引（★ PRD §4.9 幂等设计的物理基础）
--      LikeConsumer 用 INSERT IGNORE，借助 MySQL 唯一索引天然幂等：
--      重复消息 → 唯一冲突 → IGNORE → RowsAffected=0 → like_count 不增。
--      消息只投递一次的 Channel 模式靠不到这一保护，但第 13 步 RabbitMQ
--      切换后会出现重复投递，唯一索引是届时的最后一道防线。
--
--   3. idx_post_id 普通索引
--      用于"某帖被谁点过赞"反查（管理后台 / 防刷脚本场景）。
--      CountConsumer 不依赖此索引（它直接收事件聚合，不查表）。
--      MVP 不建 idx_user_id（"我点过的所有帖子"不在 P0 范围；P1 时 ALTER 加）。
--
--   4. created_at DATETIME(3)
--      毫秒精度与 posts.created_at 保持一致，方便日后做"最近点赞流"
--      游标分页（关注 Feed 用得到）。
--
--   5. 主键独立 BIGINT UNSIGNED AUTO_INCREMENT
--      没有用 (user_id, post_id) 做联合主键的原因：
--        a. 联合主键会让所有二级索引都包含 (user_id, post_id) 两列，
--           膨胀索引大小（InnoDB 二级索引尾巴存主键）。
--        b. 独立 id 主键便于未来引入"谁最先点的赞"分析、
--           "点赞撤销重做"事件追溯（用 id 比较时序比 created_at 稳）。
--
--   6. 没有 user_id / post_id 的外键约束
--      与 posts 表同样的考虑：
--        - 外键拖累批量写入性能，互联网做法是逻辑外键 + 应用层兜底。
--        - 用户/帖子被软删除后，likes 记录可保留作为历史数据；
--          删硬删时由应用层一并清理（第 19 步种子数据脚本会示例）。
--
--   7. 字符集 utf8mb4_unicode_ci，与全表统一。
--      表本身不存任何字符串列，但 default charset 一致便于跨表 JOIN。

CREATE TABLE `likes` (
  `id`         BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT             COMMENT '主键',
  `user_id`    BIGINT UNSIGNED  NOT NULL                            COMMENT '点赞者 user_id（逻辑外键 → users.id）',
  `post_id`    BIGINT UNSIGNED  NOT NULL                            COMMENT '被点赞 post_id（逻辑外键 → posts.id）',
  `created_at` DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '点赞时间，毫秒精度',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_post` (`user_id`, `post_id`),
  KEY `idx_post_id` (`post_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='点赞关系表';