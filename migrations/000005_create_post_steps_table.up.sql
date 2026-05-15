-- migrations/000005_create_post_steps_table.up.sql
--
-- Step 9 · 引入结构化步骤子表，实装 PRD-Phase2 §F-C01「步骤列表」字段。
--
-- 设计要点：
--   1) 与 posts 表 1:N 关系，post_id 不加 FK 约束 —— 项目惯例（参见
--      000003_create_likes_table 同款理由：FK 在分库分表与软删除场景下
--      约束反而成为障碍；引用完整性由 service 层在事务内保证）。
--   2) image_urls 用 JSON 列：每步至多 3 张图片 URL，service 层校验长度
--      和白名单。Go 侧通过 model.StringArray 自定义类型直接映射到 []string。
--   3) (post_id, step_no) 唯一约束：业务上"同一篇帖子不能有两个相同序号
--      的步骤"，违反则 INSERT 失败 —— DB 兜底防止并发重复提交。
--   4) idx_post 单列索引：详情页 LoadSteps 用 `WHERE post_id=? ORDER BY
--      step_no ASC`，本索引足够；uk_post_step 也能命中，但单列 idx_post
--      更通用且不依赖排序方向。
--
-- 不加的索引：
--   - 不加 created_at 索引 —— 步骤表无独立时间线查询场景，永远跟随父帖。

CREATE TABLE IF NOT EXISTS `post_steps` (
  `id`          BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT       COMMENT '主键',
  `post_id`     BIGINT           NOT NULL                      COMMENT '所属帖子 id（软引用 posts.id，无 FK）',
  `step_no`     TINYINT UNSIGNED NOT NULL                      COMMENT '步骤序号 1..30，service 层校验上限',
  `text`        VARCHAR(500)     NOT NULL DEFAULT ''           COMMENT '步骤文字说明，必填但允许空串以兼容 NOT NULL',
  `image_urls`  JSON             NOT NULL                      COMMENT '步骤图 URL 列表（[]string），至多 3 张',
  `created_at`  DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_post_step` (`post_id`, `step_no`)             COMMENT '同帖步骤唯一',
  KEY `idx_post` (`post_id`)                                    COMMENT '详情页 LoadSteps 主索引'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='帖子步骤表 — Step 9 落地，与 posts 1:N';