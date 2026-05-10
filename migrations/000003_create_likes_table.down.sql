-- migrations/000003_create_likes_table.down.sql
--
-- 回滚第 5 步：删除 likes 表。
-- 注意：执行此回滚会清除所有点赞数据。生产环境严禁运行 down 脚本，
-- 仅限开发期 reset 使用。

DROP TABLE IF EXISTS `likes`;