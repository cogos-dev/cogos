// Package clients provides client interfaces for CogOS services
package clients

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	sdk "github.com/cogos-dev/cogos/sdk"
)

// EventClient emits events to the CogOS event system
type EventClient struct {
	kernel *sdk.Kernel
}

// NewEventClient creates a new event client
func NewEventClient(k *sdk.Kernel) *EventClient {
	return &EventClient{kernel: k}
}

// Emit sends an event to the CogOS event system
// Events trigger declarative handlers defined in .cog/events/
func (c *EventClient) Emit(eventName string) error {
	return c.EmitContext(context.Background(), eventName)
}

// EmitContext sends an event with context
func (c *EventClient) EmitContext(ctx context.Context, eventName string) error {
	if eventName == "" {
		return fmt.Errorf("event name required")
	}

	// Find cog binary
	cogPath := c.kernel.Root() + "/.cog/cog"

	cmd := exec.CommandContext(ctx, cogPath, "emit", eventName)
	cmd.Dir = c.kernel.Root()

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("emit failed: %w: %s", err, strings.TrimSpace(string(output)))
	}

	return nil
}

// EmitDryRun shows what would happen without executing effects
func (c *EventClient) EmitDryRun(eventName string) (string, error) {
	return c.EmitDryRunContext(context.Background(), eventName)
}

// EmitDryRunContext shows what would happen with context
func (c *EventClient) EmitDryRunContext(ctx context.Context, eventName string) (string, error) {
	if eventName == "" {
		return "", fmt.Errorf("event name required")
	}

	cogPath := c.kernel.Root() + "/.cog/cog"

	cmd := exec.CommandContext(ctx, cogPath, "emit", eventName, "--dry-run")
	cmd.Dir = c.kernel.Root()

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("emit dry-run failed: %w: %s", err, strings.TrimSpace(string(output)))
	}

	return string(output), nil
}

// Standard event names for widgets
const (
	EventWidgetStarted   = "cog.widget.started"
	EventWidgetStopped   = "cog.widget.stopped"
	EventMessageSent     = "cog.chat.message.sent"
	EventMessageReceived = "cog.chat.message.received"
	EventIntentClassified = "cog.spark.intent.classified"
	EventInferenceStart  = "cog.inference.start"
	EventInferenceEnd    = "cog.inference.end"
	EventSignalDeposited = "cog.signal.deposited"
)
