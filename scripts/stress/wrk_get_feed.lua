-- wrk_get_feed.lua: GET /api/v1/feed
-- 用法: wrk -t4 -c50 -d30s -s wrk_get_feed.lua https://mellowck.com

wrk.method = "GET"
wrk.path   = "/api/v1/feed?size=20"
wrk.headers["Accept"] = "application/json"
