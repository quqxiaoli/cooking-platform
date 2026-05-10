// Package event defines the EventBus abstraction and its two implementations.
//
// Interfaces:
//
//	EventPublisher  — publish a typed event message
//	EventSubscriber — subscribe and consume event messages
//	EventBus        — combines both + lifecycle (Close)
//
// Implementations:
//
//	channel.go  — in-process Go channel (MVP, Step 2)
//	rabbitmq.go — RabbitMQ via amqp091-go (production, Step 13)
//
// Business code only imports the EventPublisher interface, so switching
// from channel to RabbitMQ requires zero changes to service/consumer code.
package event
