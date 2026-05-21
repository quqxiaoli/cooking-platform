-- wrk_get_detail.lua: GET /api/v1/posts/:id
-- 依赖 /tmp/stress_ctx_step20 第一行 POST_ID

local f = io.open("/tmp/stress_ctx_step20", "r")
if not f then
    error("stress context file not found: run stress_test.sh first")
end
local post_id = f:read("*l")
f:close()
if not post_id or post_id == "" then
    error("POST_ID empty in /tmp/stress_ctx_step20")
end

wrk.method = "GET"
wrk.path   = "/api/v1/posts/" .. post_id
wrk.headers["Accept"] = "application/json"
