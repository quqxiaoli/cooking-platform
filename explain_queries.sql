-- ============================================
-- cooking-platform 数据库 EXPLAIN 分析脚本
-- ============================================

-- ------------------------------
-- 1. Feed 查询分析
-- ------------------------------

-- 1.1 首页 Feed 查询 (is_visible=1, 按 created_at 倒序)
-- 目标索引: idx_visible_created (is_visible, created_at DESC)
EXPLAIN 
SELECT * FROM posts 
WHERE is_visible = 1 
ORDER BY created_at DESC 
LIMIT 20;

-- 1.2 场景 Feed 查询 (scene_tag=1, is_visible=1)
-- 目标索引: idx_scene_visible_created (scene_tag, is_visible, created_at DESC)
EXPLAIN 
SELECT * FROM posts 
WHERE scene_tag = 1 AND is_visible = 1 
ORDER BY created_at DESC 
LIMIT 20;

-- 1.3 作者主页 Feed (user_id=?, is_visible=1)
-- 目标索引: idx_user_created (user_id, created_at DESC)
EXPLAIN 
SELECT * FROM posts 
WHERE user_id = 1 AND is_visible = 1 
ORDER BY created_at DESC 
LIMIT 20;

-- ------------------------------
-- 2. 点赞查询分析
-- ------------------------------

-- 2.1 检查用户是否点赞了帖子
-- 目标索引: uk_user_post (user_id, post_id)
EXPLAIN 
SELECT * FROM likes 
WHERE user_id = 1 AND post_id = 1;

-- 2.2 查询某帖子的所有点赞
-- 目标索引: idx_post_id (post_id)
EXPLAIN 
SELECT * FROM likes 
WHERE post_id = 1;

-- ------------------------------
-- 3. 用户查询分析
-- ------------------------------

-- 3.1 按 phone_hash 查询用户 (登录场景)
-- 目标索引: uk_phone_hash (phone_hash)
EXPLAIN 
SELECT * FROM users 
WHERE phone_hash = 'abc123def456';

-- 3.2 按 ID 查询用户
-- 目标索引: PRIMARY (id)
EXPLAIN 
SELECT * FROM users 
WHERE id = 1;

-- ------------------------------
-- 4. 失效索引查询分析 (故意写的慢查询)
-- ------------------------------

-- 4.1 函数运算导致索引失效: UPPER(nickname)
-- users 表没有 nickname 索引，即使有也会因为函数运算失效
EXPLAIN 
SELECT * FROM users 
WHERE UPPER(nickname) = 'TESTUSER';

-- 4.2 类型转换导致索引失效: phone_hash 与数字比较
-- phone_hash 是 VARCHAR，这里用数字比较会触发隐式类型转换
EXPLAIN 
SELECT * FROM users 
WHERE phone_hash = 123456;

-- 4.3 函数运算导致索引失效: DATE(created_at)
-- idx_created_at 索引无法被使用
EXPLAIN 
SELECT * FROM posts 
WHERE DATE(created_at) = '2024-01-01';

-- 4.4 场景标签用字符串比较 (类型转换)
-- scene_tag 是 TINYINT，用字符串比较会触发隐式转换
EXPLAIN 
SELECT * FROM posts 
WHERE scene_tag = '1';

-- ------------------------------
-- 5. 关注查询分析
-- ------------------------------

-- 5.1 检查是否已关注
-- 目标索引: uk_follower_following (follower_id, following_id)
EXPLAIN 
SELECT * FROM follows 
WHERE follower_id = 1 AND following_id = 2;

-- 5.2 查询粉丝列表
-- 目标索引: idx_following_id (following_id)
EXPLAIN 
SELECT * FROM follows 
WHERE following_id = 1 
ORDER BY id DESC 
LIMIT 20;