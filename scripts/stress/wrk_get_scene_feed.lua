-- wrk_get_scene_feed.lua: GET /api/v1/feed?scene_tag=N
--
-- 验证按 scene_tag 过滤的 Feed 路径（命中 idx_scene_visible_created 索引）。
--
-- 不依赖 pool 文件：scene_tag ∈ 1..8 是固定 8 个值，随机选即可。
-- 若 /tmp/stress_pool_posts.tsv 存在则从池里挑实际有内容的 scene_tag，
-- 避免压到一个空场景导致永远走「无结果」分支。

local scenes = {}

function init(args)
    local f = io.open("/tmp/stress_pool_posts.tsv", "r")
    if f then
        local first = true
        local seen = {}
        for line in f:lines() do
            if first then
                first = false
            else
                local fields = {}
                for v in string.gmatch(line, "([^\t]+)") do
                    table.insert(fields, v)
                end
                local s = tonumber(fields[2] or "")
                if s and not seen[s] then
                    seen[s] = true
                    table.insert(scenes, s)
                end
            end
        end
        f:close()
    end
    if #scenes == 0 then
        -- 没有 pool 也能跑：用全部 8 个 scene_tag
        for i = 1, 8 do table.insert(scenes, i) end
    end
    math.randomseed(os.time() + tonumber(tostring({}):sub(8)) or 0)
end

function request()
    local scene = scenes[math.random(#scenes)]
    local path = "/api/v1/feed?scene_tag=" .. scene .. "&size=20"
    return wrk.format("GET", path, { ["Accept"] = "application/json" }, nil)
end
