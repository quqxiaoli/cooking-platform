-- wrk_get_user_posts.lua: GET /api/v1/users/:id/posts
--
-- 依赖：/tmp/stress_pool_users.tsv（取 user_id 列）
--
-- 每条请求随机挑一个 user_id 访问其主页帖子列表，验证「按 author 过滤
-- + 走 idx_author_visible_created 索引」的 Feed 路径性能。

local user_ids = {}

function init(args)
    local f = io.open("/tmp/stress_pool_users.tsv", "r")
    if not f then
        error("/tmp/stress_pool_users.tsv not found — run seed_stress_data.sh first")
    end
    local first = true
    for line in f:lines() do
        if first then
            first = false
        else
            local fields = {}
            for v in string.gmatch(line, "([^\t]+)") do
                table.insert(fields, v)
            end
            if fields[1] and fields[1] ~= "" then
                table.insert(user_ids, fields[1])
            end
        end
    end
    f:close()
    if #user_ids == 0 then error("user pool empty") end
    math.randomseed(os.time() + tonumber(tostring({}):sub(8)) or 0)
end

function request()
    local uid = user_ids[math.random(#user_ids)]
    local path = "/api/v1/users/" .. uid .. "/posts?size=20"
    return wrk.format("GET", path, { ["Accept"] = "application/json" }, nil)
end
