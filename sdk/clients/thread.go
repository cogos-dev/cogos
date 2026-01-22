package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cogos-dev/cogos/sdk"
	"github.com/cogos-dev/cogos/sdk/types"
)

// ThreadClient provides ergonomic access to cog://thread/*
//
// Threads are conversation histories that persist across sessions.
// They contain messages with roles (user, assistant, system, tool).
//
// All methods are goroutine-safe.
type ThreadClient struct {
	kernel *sdk.Kernel
}

// NewThreadClient creates a new ThreadClient.
func NewThreadClient(k *sdk.Kernel) *ThreadClient {
	return &ThreadClient{kernel: k}
}

// Get retrieves a thread by ID.
//
// Example:
//
//	thread, err := c.Thread.Get("abc123")
//	fmt.Printf("Thread has %d messages\n", len(thread.Messages))
func (c *ThreadClient) Get(id string) (*types.Thread, error) {
	return c.GetContext(context.Background(), id)
}

// GetContext is like Get but accepts a context.
func (c *ThreadClient) GetContext(ctx context.Context, id string) (*types.Thread, error) {
	uri := fmt.Sprintf("cog://thread/%s", id)
	resource, err := c.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	var thread types.Thread
	if err := json.Unmarshal(resource.Content, &thread); err != nil {
		return nil, fmt.Errorf("parse thread: %w", err)
	}

	return &thread, nil
}

// Current retrieves the current active thread.
//
// Example:
//
//	thread, err := c.Thread.Current()
func (c *ThreadClient) Current() (*types.Thread, error) {
	return c.CurrentContext(context.Background())
}

// CurrentContext is like Current but accepts a context.
func (c *ThreadClient) CurrentContext(ctx context.Context) (*types.Thread, error) {
	resource, err := c.kernel.ResolveContext(ctx, "cog://thread/current")
	if err != nil {
		return nil, err
	}

	var thread types.Thread
	if err := json.Unmarshal(resource.Content, &thread); err != nil {
		return nil, fmt.Errorf("parse thread: %w", err)
	}

	return &thread, nil
}

// List returns all threads (as summaries for efficiency).
//
// Example:
//
//	threads, err := c.Thread.List()
//	for _, t := range threads {
//	    fmt.Printf("%s: %d messages\n", t.ID, t.MessageCount())
//	}
func (c *ThreadClient) List() ([]*types.Thread, error) {
	return c.ListContext(context.Background())
}

// ListContext is like List but accepts a context.
func (c *ThreadClient) ListContext(ctx context.Context) ([]*types.Thread, error) {
	resource, err := c.kernel.ResolveContext(ctx, "cog://thread")
	if err != nil {
		return nil, err
	}

	// Try to parse as ThreadList first
	var threadList types.ThreadList
	if err := json.Unmarshal(resource.Content, &threadList); err == nil && len(threadList.Threads) > 0 {
		// Convert summaries to full threads (without messages)
		threads := make([]*types.Thread, len(threadList.Threads))
		for i, summary := range threadList.Threads {
			threads[i] = &types.Thread{
				ID:        summary.ID,
				Title:     summary.Title,
				UpdatedAt: summary.LastActivity,
				Status:    summary.Status,
			}
		}
		return threads, nil
	}

	// Try to parse as array of threads
	var threads []*types.Thread
	if err := json.Unmarshal(resource.Content, &threads); err != nil {
		return nil, fmt.Errorf("parse threads: %w", err)
	}

	return threads, nil
}

// Create creates a new thread with the given title.
// Returns the created thread with its assigned ID.
//
// Example:
//
//	thread, err := c.Thread.Create("Research session")
func (c *ThreadClient) Create(title string) (*types.Thread, error) {
	return c.CreateContext(context.Background(), title)
}

// CreateContext is like Create but accepts a context.
func (c *ThreadClient) CreateContext(ctx context.Context, title string) (*types.Thread, error) {
	// Generate an ID (timestamp-based)
	id := fmt.Sprintf("thread-%d", time.Now().UnixNano()/1000000)

	thread := types.NewThread(id)
	thread.Title = title

	content, err := json.Marshal(thread)
	if err != nil {
		return nil, fmt.Errorf("marshal thread: %w", err)
	}

	uri := fmt.Sprintf("cog://thread/%s", id)
	mutation := sdk.NewSetMutation(content)
	if err := c.kernel.MutateContext(ctx, uri, mutation); err != nil {
		return nil, err
	}

	return thread, nil
}

// Archive archives a thread by ID.
// Archived threads are not deleted but are marked as inactive.
//
// Example:
//
//	err := c.Thread.Archive("abc123")
func (c *ThreadClient) Archive(id string) error {
	return c.ArchiveContext(context.Background(), id)
}

// ArchiveContext is like Archive but accepts a context.
func (c *ThreadClient) ArchiveContext(ctx context.Context, id string) error {
	// Get the thread first
	thread, err := c.GetContext(ctx, id)
	if err != nil {
		return err
	}

	// Update status
	thread.Status = "archived"
	thread.UpdatedAt = time.Now()

	content, err := json.Marshal(thread)
	if err != nil {
		return fmt.Errorf("marshal thread: %w", err)
	}

	uri := fmt.Sprintf("cog://thread/%s", id)
	mutation := sdk.NewSetMutation(content)
	return c.kernel.MutateContext(ctx, uri, mutation)
}

// Delete permanently deletes a thread.
//
// Example:
//
//	err := c.Thread.Delete("abc123")
func (c *ThreadClient) Delete(id string) error {
	return c.DeleteContext(context.Background(), id)
}

// DeleteContext is like Delete but accepts a context.
func (c *ThreadClient) DeleteContext(ctx context.Context, id string) error {
	uri := fmt.Sprintf("cog://thread/%s", id)
	mutation := sdk.NewDeleteMutation()
	return c.kernel.MutateContext(ctx, uri, mutation)
}

// Append appends a message to a thread.
//
// Example:
//
//	msg := types.NewUserMessage("What is the eigenform?")
//	err := c.Thread.Append("abc123", *msg)
func (c *ThreadClient) Append(threadID string, msg types.Message) error {
	return c.AppendContext(context.Background(), threadID, msg)
}

// AppendContext is like Append but accepts a context.
func (c *ThreadClient) AppendContext(ctx context.Context, threadID string, msg types.Message) error {
	// Ensure timestamp
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	content, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	uri := fmt.Sprintf("cog://thread/%s", threadID)
	mutation := sdk.NewAppendMutation(content)
	return c.kernel.MutateContext(ctx, uri, mutation)
}

// AppendUser is a convenience method to append a user message.
//
// Example:
//
//	err := c.Thread.AppendUser("abc123", "What is the eigenform?")
func (c *ThreadClient) AppendUser(threadID, content string) error {
	return c.Append(threadID, *types.NewUserMessage(content))
}

// AppendAssistant is a convenience method to append an assistant message.
//
// Example:
//
//	err := c.Thread.AppendAssistant("abc123", "The eigenform is...")
func (c *ThreadClient) AppendAssistant(threadID, content string) error {
	return c.Append(threadID, *types.NewAssistantMessage(content))
}

// AppendSystem is a convenience method to append a system message.
//
// Example:
//
//	err := c.Thread.AppendSystem("abc123", "Session started")
func (c *ThreadClient) AppendSystem(threadID, content string) error {
	return c.Append(threadID, *types.NewSystemMessage(content))
}

// LastN returns the last N messages from a thread.
//
// Example:
//
//	messages, err := c.Thread.LastN("abc123", 5)
func (c *ThreadClient) LastN(threadID string, n int) ([]*types.Message, error) {
	return c.LastNContext(context.Background(), threadID, n)
}

// LastNContext is like LastN but accepts a context.
func (c *ThreadClient) LastNContext(ctx context.Context, threadID string, n int) ([]*types.Message, error) {
	uri := fmt.Sprintf("cog://thread/%s#last-%d", threadID, n)
	resource, err := c.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	// Try to parse as messages array
	var messages []*types.Message
	if err := json.Unmarshal(resource.Content, &messages); err == nil {
		return messages, nil
	}

	// Fall back to getting full thread and taking last N
	thread, err := c.GetContext(ctx, threadID)
	if err != nil {
		return nil, err
	}

	return thread.LastN(n), nil
}

// Messages returns all messages in a thread.
//
// Example:
//
//	messages, err := c.Thread.Messages("abc123")
func (c *ThreadClient) Messages(threadID string) ([]*types.Message, error) {
	return c.MessagesContext(context.Background(), threadID)
}

// MessagesContext is like Messages but accepts a context.
func (c *ThreadClient) MessagesContext(ctx context.Context, threadID string) ([]*types.Message, error) {
	thread, err := c.GetContext(ctx, threadID)
	if err != nil {
		return nil, err
	}

	return thread.Messages, nil
}

// Count returns the number of messages in a thread.
func (c *ThreadClient) Count(threadID string) (int, error) {
	thread, err := c.Get(threadID)
	if err != nil {
		return 0, err
	}
	return thread.MessageCount(), nil
}

// IsActive returns true if the thread is active (not archived).
func (c *ThreadClient) IsActive(threadID string) (bool, error) {
	thread, err := c.Get(threadID)
	if err != nil {
		return false, err
	}
	return thread.IsActive(), nil
}

// SetTitle sets the title of a thread.
func (c *ThreadClient) SetTitle(threadID, title string) error {
	return c.SetTitleContext(context.Background(), threadID, title)
}

// SetTitleContext is like SetTitle but accepts a context.
func (c *ThreadClient) SetTitleContext(ctx context.Context, threadID, title string) error {
	thread, err := c.GetContext(ctx, threadID)
	if err != nil {
		return err
	}

	thread.Title = title
	thread.UpdatedAt = time.Now()

	content, err := json.Marshal(thread)
	if err != nil {
		return fmt.Errorf("marshal thread: %w", err)
	}

	uri := fmt.Sprintf("cog://thread/%s", threadID)
	mutation := sdk.NewSetMutation(content)
	return c.kernel.MutateContext(ctx, uri, mutation)
}
