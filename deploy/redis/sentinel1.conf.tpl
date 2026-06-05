## Sentinel 1/3 — single-host quorum=2 topology.
##
## envsubst at container startup replaces ${REDIS_PASSWORD} with the value
## from .env.prod, then writes the rendered file to a sentinel-owned location
## (sentinel rewrites this file at runtime to record known-replica /
## current-epoch state — so each sentinel needs its own writable conf).
##
## Layout (all on one host, isolated by ports):
##   redis-master       6379  → data
##   redis-slave        6380  → data
##   redis-sentinel1   26379  → control plane
##   redis-sentinel2   26380  → control plane
##   redis-sentinel3   26381  → control plane
##
## quorum=2 means at least 2 sentinels must agree before failover triggers,
## so any single sentinel crash does NOT cause spurious promotion.

port 26379
sentinel resolve-hostnames yes
sentinel announce-hostnames yes

## down-after-milliseconds 5000 — master must be unreachable for 5s before
## sentinels start failover negotiation. Shorter values cause flapping
## during transient network blips.
sentinel monitor mymaster redis-master 6379 2
sentinel down-after-milliseconds mymaster 5000
sentinel failover-timeout mymaster 30000
sentinel parallel-syncs mymaster 1

sentinel auth-pass mymaster ${REDIS_PASSWORD}
requirepass ${REDIS_PASSWORD}
