-- wrk_get_search.lua: GET /api/v1/search?q=<keyword>&size=20
--
-- 关键字池：与 scripts/stress/seed_stress_data.sh 写入帖子标题用的 5 个关键字
-- 保持一致；每次请求随机选一个，避免单关键字 hot-key 命中应用层限流
-- （详见 code_issues_log.md #C2）。
--
-- 注：即便随机化后，如果 search 服务对 q 实际值无关、按调用方限流，
-- 这个测试场景的 non-2xx 仍会偏高，到时直接接受降级即可。

-- 注：去掉 "搜索"（UTF-8 非 ASCII），wrk url parser 在 HTTP/1.1 URI 上不接受
-- 多字节字符（"invalid URL at 1:22"）。如需测中文搜索路径，得改成 percent-encode。
local keywords = { "verify", "stress", "test", "feed", "post" }

function init(args)
    math.randomseed(os.time() + tonumber(tostring({}):sub(8)) or 0)
end

function request()
    local q = keywords[math.random(#keywords)]
    return wrk.format("GET", "/api/v1/search?q=" .. q .. "&size=20",
                      { ["Accept"] = "application/json" }, nil)
end
