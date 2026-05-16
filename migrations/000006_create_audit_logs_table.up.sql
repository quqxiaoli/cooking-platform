-- 000006_create_audit_logs_table.up.sql
--
-- audit_logs 是审核事件的不可变追加日志。每次状态流转写一行，
-- 已写行永不更新——合规审计需要完整的状态变更历史。
--
-- post_id / author_id 做冗余快照而非外键：帖子/用户软删除后
-- 审核记录仍可独立查询，满足内容安全合规存档要求（PRD §10）。
--
-- raw_response 存阿里云 API 原始 JSON，供事后人工核查
-- 机审结论是否合理，不做任何截断（TEXT 最大 65535 字节，足够）。

CREATE TABLE `audit_logs` (
  `id`           BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `post_id`      BIGINT UNSIGNED  NOT NULL                 COMMENT '被审帖子 ID（冗余快照，无外键约束）',
  `author_id`    BIGINT UNSIGNED  NOT NULL                 COMMENT '帖子作者 ID（冗余快照）',
  `audit_status` TINYINT UNSIGNED NOT NULL                 COMMENT '0=待审 1=机审通过 2=疑似 3=人工通过 4=机审拒绝 5=人工拒绝',
  `remark`       VARCHAR(500)     NOT NULL DEFAULT ''      COMMENT '审核备注或拒绝原因摘要',
  `raw_response` TEXT             NOT NULL                 COMMENT '审核提供方原始 JSON，用于事后核查',
  `created_at`   DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  KEY `idx_post_id`    (`post_id`),
  KEY `idx_created_at` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='内容审核事件日志（只追加，不更新）';
