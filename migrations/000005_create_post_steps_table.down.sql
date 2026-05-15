-- migrations/000005_create_post_steps_table.down.sql
--
-- 反向迁移：删除 post_steps 表。
-- 注意：down 不会反向写 posts.content 字段（content 列在 Step 4 已存在，
-- 与本步无关），故仅删除新增子表。
DROP TABLE IF EXISTS `post_steps`;