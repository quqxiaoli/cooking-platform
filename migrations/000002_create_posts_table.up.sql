-- migrations/000002_create_posts_table.up.sql
--
-- 内容表（posts）：MVP 文字版的核心实体。
--
-- 设计要点（与 PRD-Phase3 v2.1 §5.2 / §5.3 对齐）：
--
--   1. is_visible 冗余字段（★ PRD-Phase3 修复 #5）
--      Feed 查询条件 `WHERE is_visible=1 AND created_at < ?` 是单值等值
--      匹配 + 范围扫描，索引命中率 100%。若直接用 audit_status IN (1,3)
--      复合索引会做多次扫描合并，深翻页触发 filesort。
--      MVP（第 4 步）发帖直接置 1；第 10 步审核 Consumer 接入后由其维护。
--
--   2. audit_status 6 态状态机（与 §9.1 一致）：
--      0=待审 1=机审通过 2=疑似 3=人工通过 4=机审拒绝 5=人工拒绝
--      MVP 阶段 audit_status 始终为 0，第 10 步起进入完整状态流转。
--
--   3. content TEXT 列：
--      ★ 偏离 PRD-Phase3 §5.2（PRD 列定义里没有 content 列）
--      MVP 文字版需要正文存储。第 9 步引入图片上传后，若需拆出 post_steps
--      子表（结构化步骤图文），届时迁移路径：
--        - 新增 migration 创建 post_steps，content 字段保留为兼容字段
--        - 或将 content 整体迁入 post_steps.text_only_legacy，content 软废弃
--
--   4. 时间字段 DATETIME(3) 毫秒精度：
--      游标分页基于 created_at 比较，毫秒精度避免同秒多帖的游标冲突。
--
--   5. 复合索引设计（与 §5.3 索引策略对齐）：
--      - idx_user_created             → 作者主页查询（WHERE user_id=? ORDER BY created_at DESC）
--      - idx_visible_created          → 首页 Feed（WHERE is_visible=1 ORDER BY created_at DESC）
--      - idx_scene_visible_created    → 按场景 Feed（WHERE scene_tag=? AND is_visible=1）
--      - idx_audit_status             → 后台审核列表（管理端用）
--      索引顺序：等值列在前，范围列在后，是经典最左前缀原则。
--
--   6. FULLTEXT KEY WITH PARSER ngram（第 7 步搜索用）：
--      MySQL 8.0 内置 ngram 解析器，支持中文按 N 字切分（默认 N=2）。
--      MVP 提前建好索引，避免后续 ALTER TABLE 锁表（百万级数据时建索引会卡）。
--      搜索改 ES 的触发条件：MAU 50 万 + P95 搜索 > 500ms。
--
--   7. 字符集 utf8mb4_unicode_ci，与 users 表保持一致。
--
--   8. 没有 user_id 外键约束：
--      用户软删除后 posts 仍要可见（孤立内容由 PVConsumer 负责跳过显示作者）。
--      外键还会拖累批量写入性能，互联网常规做法是逻辑外键 + 应用层兜底。

CREATE TABLE `posts` (
  `id`             BIGINT UNSIGNED   NOT NULL AUTO_INCREMENT,
  `user_id`        BIGINT UNSIGNED   NOT NULL                COMMENT '作者 user_id（关联 users.id，逻辑外键）',
  `title`          VARCHAR(100)      NOT NULL                COMMENT '标题，1-100 字符',
  `content`        TEXT              NULL                    COMMENT '正文（MVP 文字版，最长 5000 字符）；★ PRD §5.2 未列，第 4 步引入',
  `scene_tag`      TINYINT UNSIGNED  NOT NULL                COMMENT '1=出租屋 2=一个人的饭 3=露营野炊 4=家庭厨房 5=快手日常 6=打包便当 7=减脂餐 8=节气节日',
  `cook_duration`  TINYINT UNSIGNED  NOT NULL DEFAULT 0      COMMENT '0=未填 1=15分钟内 2=30分钟内 3=1小时内 4=1小时以上',
  `cover_url`      VARCHAR(500)      NOT NULL DEFAULT ''     COMMENT 'OSS 封面 URL（第 9 步图片上传后填充）',
  `like_count`     INT UNSIGNED      NOT NULL DEFAULT 0      COMMENT '点赞数（LikeConsumer 异步同步，第 5 步起）',
  `view_count`     INT UNSIGNED      NOT NULL DEFAULT 0      COMMENT '浏览量（PVConsumer 异步同步，第 5 步起）',
  `is_visible`     TINYINT UNSIGNED  NOT NULL DEFAULT 0      COMMENT '0=不可见 1=可见（审核通过时由 AuditConsumer 置 1）',
  `audit_status`   TINYINT UNSIGNED  NOT NULL DEFAULT 0      COMMENT '0=待审 1=机审通过 2=疑似 3=人工通过 4=机审拒绝 5=人工拒绝',
  `audit_remark`   VARCHAR(200)      NOT NULL DEFAULT ''     COMMENT '审核备注（拒绝原因等）',
  `created_at`     DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`     DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  `deleted_at`     DATETIME(3)       NULL DEFAULT NULL       COMMENT '软删除时间戳（GORM 自动维护）',
  PRIMARY KEY (`id`),
  KEY `idx_user_created` (`user_id`, `created_at` DESC),
  KEY `idx_visible_created` (`is_visible`, `created_at` DESC),
  KEY `idx_scene_visible_created` (`scene_tag`, `is_visible`, `created_at` DESC),
  KEY `idx_audit_status` (`audit_status`, `created_at` DESC),
  FULLTEXT KEY `ft_title` (`title`) WITH PARSER ngram
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='内容表（菜谱/烹饪笔记）';