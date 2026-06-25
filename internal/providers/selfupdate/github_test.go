package selfupdate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingTransport counts requests and returns a canned latest-release body.
type countingTransport struct {
	count int32
	tag   string
	fail  bool
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&c.count, 1)
	if c.fail {
		return nil, fmt.Errorf("simulated network failure")
	}
	body := fmt.Sprintf(`{"tag_name":%q,"prerelease":false,"draft":false}`, c.tag)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func withTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := httpClientFor
	httpClientFor = func() *http.Client { return &http.Client{Transport: rt} }
	t.Cleanup(func() { httpClientFor = prev })
}

func TestResolveThrottlesWithinInterval(t *testing.T) {
	ct := &countingTransport{tag: "v0.16.5"}
	withTransport(t, ct)

	r := &ReleaseResolver{}
	cfg := &SelfUpdateConfig{Enabled: true, Channel: channelStable, Repo: defaultRepo, CheckInterval: time.Hour}

	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(context.Background(), cfg); err != nil {
			t.Fatalf("Resolve #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&ct.count); got != 1 {
		t.Errorf("within-interval request count = %d; want 1", got)
	}
}

func TestResolveRefetchesAfterInterval(t *testing.T) {
	ct := &countingTransport{tag: "v0.16.5"}
	withTransport(t, ct)

	r := &ReleaseResolver{}
	cfg := &SelfUpdateConfig{Enabled: true, Channel: channelStable, Repo: defaultRepo, CheckInterval: time.Hour}

	if _, err := r.Resolve(context.Background(), cfg); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Force the cache to look old.
	r.mu.Lock()
	r.cachedAt = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	if _, err := r.Resolve(context.Background(), cfg); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := atomic.LoadInt32(&ct.count); got != 2 {
		t.Errorf("post-interval request count = %d; want 2", got)
	}
}

func TestResolveServesStaleOnTransientError(t *testing.T) {
	ct := &countingTransport{tag: "v0.16.5"}
	withTransport(t, ct)

	r := &ReleaseResolver{}
	cfg := &SelfUpdateConfig{Enabled: true, Channel: channelStable, Repo: defaultRepo, CheckInterval: time.Hour}

	rel, err := r.Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	firstCachedAt := r.cachedAt

	// Age the cache past the interval but within 2× so a fetch is attempted and
	// stale-serve kicks in on failure.
	r.mu.Lock()
	r.cachedAt = time.Now().Add(-90 * time.Minute)
	r.mu.Unlock()
	ct.fail = true

	rel2, err := r.Resolve(context.Background(), cfg)
	if err != nil {
		t.Fatalf("expected stale serve, got error: %v", err)
	}
	if rel2.Tag != rel.Tag {
		t.Errorf("stale tag = %q; want %q", rel2.Tag, rel.Tag)
	}
	// cachedAt must NOT advance on error.
	if !r.cachedAt.Equal(time.Now().Add(-90*time.Minute)) && r.cachedAt.After(firstCachedAt) {
		// We deliberately set cachedAt to -90m above; a successful refresh would
		// have moved it to ~now. Assert it did not.
		if time.Since(r.cachedAt) < time.Minute {
			t.Error("cachedAt advanced on a failed fetch")
		}
	}
}
