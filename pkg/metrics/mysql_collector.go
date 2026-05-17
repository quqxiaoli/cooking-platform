package metrics

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// MySQLPoolCollector is a prometheus.Collector that samples connection-pool
// statistics from a *sql.DB on every Prometheus scrape (pull model).
//
// This avoids the push-polling goroutine pattern: Prometheus itself drives
// the sampling frequency, and we never hold stale data.
//
// Register with:
//
//	prometheus.MustRegister(metrics.NewMySQLPoolCollector(namespace, sqlDB))
type MySQLPoolCollector struct {
	db    *sql.DB
	open  *prometheus.Desc // open connections (in-use + idle)
	inuse *prometheus.Desc // connections currently executing a query
	idle  *prometheus.Desc // idle connections in the pool
	wait  *prometheus.Desc // total times the pool blocked waiting for a connection
}

// NewMySQLPoolCollector creates a collector for the given *sql.DB.
// The namespace is prepended to every metric name, matching metrics.Init().
func NewMySQLPoolCollector(namespace string, db *sql.DB) *MySQLPoolCollector {
	fqn := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(namespace+"_"+name, help, nil, nil)
	}
	return &MySQLPoolCollector{
		db:    db,
		open:  fqn("mysql_pool_open_connections", "Number of open MySQL connections (in-use + idle)."),
		inuse: fqn("mysql_pool_inuse_connections", "Number of MySQL connections currently executing a query."),
		idle:  fqn("mysql_pool_idle_connections", "Number of idle MySQL connections waiting in the pool."),
		wait:  fqn("mysql_pool_wait_total", "Total times the pool blocked waiting for a free connection."),
	}
}

// Describe sends the descriptor of each metric to the channel.
func (c *MySQLPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.open
	ch <- c.inuse
	ch <- c.idle
	ch <- c.wait
}

// Collect reads sql.DBStats and emits one metric per descriptor.
// Called synchronously on every Prometheus scrape — no goroutine needed.
func (c *MySQLPoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.db.Stats()
	ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, float64(s.OpenConnections))
	ch <- prometheus.MustNewConstMetric(c.inuse, prometheus.GaugeValue, float64(s.InUse))
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(s.Idle))
	ch <- prometheus.MustNewConstMetric(c.wait, prometheus.CounterValue, float64(s.WaitCount))
}
