package gateway

import "github.com/jonahgcarpenter/oswald-ai/internal/gateway/broker"

// Service is the contract every gateway implementation must satisfy.
// Start receives a Broker for submitting agent requests and should block
// for the lifetime of the gateway.
type Service interface {
	// Start should block and run the gateway.
	// Returning an error means it crashed or failed to start.
	Start(b *broker.Broker) error
	// Name returns the human-readable name of the gateway implementation.
	Name() string
}
