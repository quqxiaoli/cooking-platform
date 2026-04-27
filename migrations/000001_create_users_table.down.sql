-- migrations/000001_create_users_table.down.sql
--
-- 回滚脚本：删除 users 表。
-- 注意：down 操作会丢失所有用户数据，仅在 dev 环境或灾难恢复时使用。

DROP TABLE IF EXISTS `users`;