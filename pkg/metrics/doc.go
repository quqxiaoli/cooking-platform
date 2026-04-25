// Package metrics registers Prometheus metrics and exposes the /metrics handler.
// Metric families (added in Step 16 — observability):
//   http_requests_total        counter   method, path, status
//   http_request_duration_secs histogram method, path
//   db_query_duration_secs     histogram operation, table
//   redis_operations_total     counter   operation, status
//   mq_messages_published_total counter  topic
//   mq_messages_consumed_total counter  topic, status
package metrics
