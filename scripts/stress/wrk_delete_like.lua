-- wrk_delete_like.lua: DELETE /api/v1/posts/:id/like
--
-- 依赖：/tmp/stress_pool_users.tsv + /tmp/stress_pool_posts.tsv
-- 由 scripts/stress/seed_stress_data.sh 生成。
--
-- 每条请求随机挑一个 (user_token, post_id) 组合，避免热点偏置。
-- DELETE like 是幂等接口，重复请求只是测请求路径与 cache invalidation 开销，
-- 不验证状态正确性（service 层已有点赞模块单测覆盖）。

local users = {}   -- list of tokens
local posts = {}   -- list of post_ids

local function load_tsv(path, col_idx)
    local f = io.open(path, "r")
    if not f then
        error("stress pool file not found: " .. path .. " — run seed_stress_data.sh first")
    end
    local first = true
    local out = {}
    for line in f:lines() do
        if first then
            first = false  -- skip header
        else
            local fields = {}
            for v in string.gmatch(line, "([^\t]+)") do
                table.insert(fields, v)
            end
            if fields[col_idx] and fields[col_idx] ~= "" then
                table.insert(out, fields[col_idx])
            end
        end
    end
    f:close()
    return out
end

function init(args)
    -- users.tsv columns: user_id\ttoken
    users = load_tsv("/tmp/stress_pool_users.tsv", 2)
    -- posts.tsv columns: post_id\tscene_tag\tauthor_user_id
    posts = load_tsv("/tmp/stress_pool_posts.tsv", 1)
    if #users == 0 then error("user pool empty") end
    if #posts == 0 then error("post pool empty") end
    -- 每线程独立随机种子（os.time + thread addr-ish）
    math.randomseed(os.time() + tonumber(tostring({}):sub(8)) or 0)
end

function request()
    local token   = users[math.random(#users)]
    local post_id = posts[math.random(#posts)]
    local headers = {
        ["Authorization"] = "Bearer " .. token,
        ["Content-Type"]  = "application/json",
    }
    return wrk.format("DELETE", "/api/v1/posts/" .. post_id .. "/like", headers, nil)
end
