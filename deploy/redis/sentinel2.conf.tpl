## Sentinel 2/3 — see sentinel1.conf.tpl for full topology rationale.
## Differs only in port (26380) so it can co-exist on the same host.

port 26380
sentinel resolve-hostnames yes
sentinel announce-hostnames yes

sentinel monitor mymaster redis-master 6379 2
sentinel down-after-milliseconds mymaster 5000
sentinel failover-timeout mymaster 30000
sentinel parallel-syncs mymaster 1

sentinel auth-pass mymaster ${REDIS_PASSWORD}
requirepass ${REDIS_PASSWORD}
