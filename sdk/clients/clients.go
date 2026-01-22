// Package clients provides ergonomic convenience wrappers over the SDK kernel.
//
// Instead of working with raw URIs and Resources, clients provide typed methods
// for common operations. Each client wraps the kernel's Resolve/Mutate API
// and parses Resources into strongly-typed structs.
//
// Example usage:
//
//	kernel, _ := sdk.Connect(".")
//	defer kernel.Close()
//
//	c := clients.New(kernel)
//
//	// Read a cogdoc
//	doc, err := c.Memory.GetCogdoc("semantic/insights/eigenform")
//
//	// Deposit a signal
//	err = c.Signal.Deposit(types.Signal{Location: "inference", Type: "ACTIVE"})
//
//	// Get context for inference
//	ctx, err := c.Context.Build()
package clients

import (
	"github.com/cogos-dev/cogos/sdk"
)

// Clients provides access to all convenience clients.
// It is the primary entry point for ergonomic SDK usage.
//
// Thread-safety: All clients are goroutine-safe because the underlying
// kernel is goroutine-safe.
type Clients struct {
	// Memory provides ergonomic access to cog://mem/*
	Memory *MemoryClient

	// Signal provides ergonomic access to cog://signals/*
	Signal *SignalClient

	// Thread provides ergonomic access to cog://thread/*
	Thread *ThreadClient

	// Inference provides ergonomic access to cog://inference
	Inference *InferenceClient

	// Context provides context building for inference
	Context *ContextClient

	// Event provides event emission to cog://events
	Event *EventClient

	// kernel is the underlying kernel (kept for direct access if needed)
	kernel *sdk.Kernel
}

// New creates all clients from a kernel.
//
// The kernel must be connected (via sdk.Connect). All clients share
// the same kernel and are safe for concurrent use.
//
// Example:
//
//	kernel, err := sdk.Connect(".")
//	if err != nil {
//	    return err
//	}
//	defer kernel.Close()
//
//	c := clients.New(kernel)
//	// Use c.Memory, c.Signal, etc.
func New(k *sdk.Kernel) *Clients {
	return &Clients{
		Memory:    NewMemoryClient(k),
		Signal:    NewSignalClient(k),
		Thread:    NewThreadClient(k),
		Inference: NewInferenceClient(k),
		Context:   NewContextClient(k),
		Event:     NewEventClient(k),
		kernel:    k,
	}
}

// Kernel returns the underlying kernel for direct access.
// Use this when you need to perform operations not covered by the clients.
func (c *Clients) Kernel() *sdk.Kernel {
	return c.kernel
}

// Close closes the underlying kernel.
// After Close, all client operations will fail.
func (c *Clients) Close() error {
	return c.kernel.Close()
}
