-- wrk_post_like.lua: POST /api/v1/posts/:id/like
-- 依赖 /tmp/stress_ctx_step20: 第一行 POST_ID，第二行 TOKEN_2

local f = io.open("/tmp/stress_ctx_step20", "r")
if not f then
    error("stress context file not found: run stress_test.sh first")
end
local post_id = f:read("*l")
local token_2 = f:read("*l")
f:close()
if not post_id or post_id == "" then
    error("POST_ID empty")
end
if not token_2 or token_2 == "" then
    error("TOKEN_2 empty")
end

wrk.method = "POST"
wrk.path   = "/api/v1/posts/" .. post_id .. "/like"
wrk.headers["Authorization"] = "Bearer " .. token_2
wrk.headers["Content-Type"] = "application/json"
