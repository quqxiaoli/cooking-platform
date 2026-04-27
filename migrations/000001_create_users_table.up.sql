-- migrations/000001_create_users_table.up.sql
--
-- 创建 users 表。
--
-- 字段设计要点：
--   1. phone_encrypted：第 3 步起先存明文手机号（如 "13800138000"），
--      第 11 步迁移到 AES-GCM 密文。字段长度 200 是为了容纳密文 + base64 编码后的额外开销。
--   2. phone_hash：SHA256(phone) 的十六进制字符串（64 字符）。
--      建唯一索引 uk_phone_hash，登录时 O(1) 查找用户。
--   3. 计数字段（post_count / total_likes / follower_count / following_count）
--      由异步 Consumer 维护（CountConsumer，第 5 步起接入），
--      用户读 Profile 时直接读这些冗余字段，避免实时 COUNT。
--   4. status：0=正常，1=封禁。封禁用户的发帖、登录均被拒绝。
--   5. deleted_at：软删除时间戳，配合 GORM 的 soft delete。
--      索引不包含 deleted_at，因为业务侧通过 GORM 自动过滤。
--   6. 时间字段使用 DATETIME(3)：毫秒精度，游标分页（基于 created_at）需要。
--   7. 字符集 utf8mb4_unicode_ci：统一全表，与 PRD-Phase3 §5.1 一致。

CREATE TABLE `users` (
  `id`               BIGINT UNSIGNED   NOT NULL AUTO_INCREMENT,
  `phone_hash`       VARCHAR(64)       NOT NULL COMMENT '手机号 SHA256 十六进制哈希，登录索引',
  `phone_encrypted`  VARCHAR(200)      NOT NULL COMMENT '手机号：第 3 步存明文，第 11 步迁移为 AES-GCM 密文',
  `nickname`         VARCHAR(50)       NOT NULL DEFAULT '' COMMENT '昵称，最长 50 字符',
  `avatar_url`       VARCHAR(500)      NOT NULL DEFAULT '' COMMENT 'OSS 头像 URL',
  `bio`              VARCHAR(200)      NOT NULL DEFAULT '' COMMENT '个人简介，最长 200 字符',
  `status`           TINYINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '0=正常 1=封禁',
  `post_count`       INT UNSIGNED      NOT NULL DEFAULT 0 COMMENT '发帖总数（CountConsumer 异步同步）',
  `total_likes`      INT UNSIGNED      NOT NULL DEFAULT 0 COMMENT '收到的总点赞数（CountConsumer 异步同步）',
  `follower_count`   INT UNSIGNED      NOT NULL DEFAULT 0 COMMENT '粉丝数（CountConsumer 异步同步）',
  `following_count`  INT UNSIGNED      NOT NULL DEFAULT 0 COMMENT '关注数（CountConsumer 异步同步）',
  `created_at`       DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`       DATETIME(3)       NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  `deleted_at`       DATETIME(3)       NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_phone_hash` (`phone_hash`),
  KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='用户表';