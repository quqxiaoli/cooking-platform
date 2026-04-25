// Package service contains the business logic layer.
// Services are called by handlers and depend on repositories and the event bus.
// Each service receives a context.Context as first argument for timeout propagation.
// Transaction control lives here — never in handlers or repositories.
package service
