-- migrations/000004_create_follows_table.up.sql
--
-- 关注关系表（follows）：MVP 关注模块 F-F01 的核心实体。
--
-- 设计要点（与 PRD-Phase3 v2.1 §5.2 对齐，DDL 逐列照搬）：
--
--   1. 无 deleted_at 列（★ 与 likes 表同样的刻意偏离）
--      "取消关注 = 物理删除该行"是关注场景下更合适的语义：
--        a. 业务上没有"已取消但要审计关注历史"的需求，删除即遗忘。
--        b. 软删除会破坏 uk_follower_following 唯一约束 —— 同一
--           (follower_id, following_id) 关注又取消后再次关注，会因残留的
--           soft-deleted 行触发唯一索引冲突；除非把 deleted_at 也加进唯一
--           索引，那会让索引体积 / 维护成本 / 查询计划全面劣化。
--      users / posts 保留 deleted_at 是因为它们承载账户 / 内容审计。
--
--   2. 无 updated_at 列（★ 与 likes 表同理）
--      关注关系行不可变：要么存在（= 已关注），要么不存在（= 未关注），
--      没有任何字段会被 UPDATE。updated_at 是无意义字段。
--
--   3. uk_follower_following 唯一索引 (follower_id, following_id)
--      —— 幂等的物理基础。FollowService 用 INSERT IGNORE 写入：重复关注
--      → 唯一冲突 → IGNORE → RowsAffected=0 → 不重复发 FollowEvent、不重复
--      +1 计数。第 13 步切 RabbitMQ 后会出现重复投递，唯一索引是届时的
--      最后一道防线。它同时是"我是否已关注 TA"判断（Exists）的查询索引。
--
--   4. idx_following_id 普通索引 (following_id)
--      用于"谁关注了 TA"反查 —— 粉丝列表 ListFollowers 的驱动索引。
--      InnoDB 二级索引隐式带主键，故 idx_following_id 实际是
--      (following_id, id)，对 "WHERE following_id=? AND id<? ORDER BY id
--      DESC" 的 keyset 游标分页天然高效。
--      "我关注了谁"（ListFollowing）反查走 uk_follower_following 的
--      follower_id 前缀；其 keyset 列序非完美覆盖，但有 3000 关注上限兜底
--      （单用户扫描集 ≤ 3000 行），MVP 完全可接受。
--
--   5. created_at DATETIME(3) 毫秒精度，与全表统一。本表游标分页不依赖
--      created_at（用自增 id 做 keyset 更稳，无同毫秒并列问题），毫秒精度
--      仅为日后"最近关注流"之类时序分析预留。
--
--   6. 主键独立 BIGINT UNSIGNED AUTO_INCREMENT，不用 (follower_id,
--      following_id) 联合主键：
--        a. 联合主键会让所有二级索引尾部都带这两列，膨胀索引体积。
--        b. 独立自增 id 是关注 / 粉丝列表 keyset 游标的理想载体 ——
--           单调、稳定、唯一，无 created_at 的同毫秒 tie 问题。
--
--   7. 没有 follower_id / following_id 外键约束，与 posts / likes 一致：
--      外键拖累批量写入、与互联网"逻辑外键 + 应用层兜底"惯例相悖。用户
--      注销后 follows 残留行由应用层处理（查询时 JOIN ... AND
--      users.deleted_at IS NULL 过滤掉注销用户；第 19 步种子 / 清理脚本
--      做物理清理）。
--
--   8. 字符集 utf8mb4_unicode_ci，与全表统一（本表无字符串列，仅为跨表
--      JOIN 一致性）。

CREATE TABLE `follows` (
  `id`            BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT              COMMENT '主键',
  `follower_id`   BIGINT UNSIGNED  NOT NULL                             COMMENT '关注发起者 user_id（逻辑外键 → users.id）',
  `following_id`  BIGINT UNSIGNED  NOT NULL                             COMMENT '被关注者 user_id（逻辑外键 → users.id）',
  `created_at`    DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '关注建立时间，毫秒精度',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_follower_following` (`follower_id`, `following_id`),
  KEY `idx_following_id` (`following_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='关注关系表';