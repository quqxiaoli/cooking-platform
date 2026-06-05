-- wrk_post_follow.lua: POST /api/v1/users/:id/follow
--
-- 依赖：/tmp/stress_pool_users.tsv（提供 follower token 与 followee user_id）
--
-- 每条请求随机挑「follower != followee」的两位用户，模拟真实社交图扩散。
-- follow 接口幂等（已关注再点不报错），重复请求测请求路径与 cache 写开销。

local user_ids = {}  -- [{id, token}]

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
            if fields[1] and fields[2] then
                table.insert(user_ids, { id = fields[1], token = fields[2] })
            end
        end
    end
    f:close()
    if #user_ids < 2 then error("user pool size < 2, cannot pick follower != followee") end
    math.randomseed(os.time() + tonumber(tostring({}):sub(8)) or 0)
end

function request()
    local follower_idx = math.random(#user_ids)
    local followee_idx
    repeat
        followee_idx = math.random(#user_ids)
    until followee_idx ~= follower_idx
    local follower = user_ids[follower_idx]
    local followee = user_ids[followee_idx]
    local path = "/api/v1/users/" .. followee.id .. "/follow"
    local headers = {
        ["Authorization"] = "Bearer " .. follower.token,
        ["Content-Type"]  = "application/json",
    }
    return wrk.format("POST", path, headers, nil)
end
