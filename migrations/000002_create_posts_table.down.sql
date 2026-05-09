-- migrations/000002_create_posts_table.down.sql
--
-- 回滚第 4 步：删除 posts 表。
-- 注意：执行此回滚会清除所有内容数据，且 FULLTEXT 索引重建在数据量大时
-- 耗时显著。生产环境严禁运行 down 脚本，仅限开发期 reset 使用。

DROP TABLE IF EXISTS `posts`;