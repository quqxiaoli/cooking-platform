-- wrk_get_search.lua: GET /api/v1/search?q=...
wrk.method = "GET"
wrk.path   = "/api/v1/search?q=verify&size=20"
wrk.headers["Accept"] = "application/json"
