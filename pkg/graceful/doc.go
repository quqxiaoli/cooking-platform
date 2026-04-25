// Package graceful provides helpers for clean server shutdown.
// The main shutdown sequence lives in cmd/server/main.go;
// this package will extract reusable utilities (e.g. RunWithContext,
// WaitForSignal) if the sequence grows complex.
// Currently a placeholder — will be populated in Step 13 (RabbitMQ switch)
// when Consumer shutdown sequencing becomes non-trivial.
package graceful
