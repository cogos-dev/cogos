// zz_advr4_repair_test.go — regression tests for the #556 adversarial
// review round-4 repair: the five findings from the round-4 review
// (BLOCKING queue-endpoint credential leak, MAJOR concurrency ratchet,
// MINOR IPv6 bracket mangling, MINOR endpointless-entry dedup miss, MINOR
// declared-vs-declared last-writer-wins flap). Named zz_ so it sorts after
// the substantive files it exercises, matching the round-3 convention
// (zz_advr3_test.go, now folded into the production comments it verified).
package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── finding 1 (BLOCKING): GET /v1/queue must not leak the raw registry key ──

// TestAdvR4_QueueEndpointLeaksCredentialsOnUnauthGET locks in the fix for
// the round-4 BLOCKING finding: queueSnapshot.Endpoint used to be q.key
// verbatim, which can carry URL userinfo credentials and a path for an
// operator-configured local backend (e.g.
// "http://svc:s3cr3t-token@10.1.2.3:1234/backend-path"). GET /v1/queue is
// grant-exempt (isGrantExemptRequest exempts every GET), so that would be
// published to any unauthenticated caller who can reach the port. Asserts
// the redacted response contains no "@" (userinfo separator), no path, no
// scheme, and no credential substring — only host:port.
func TestAdvR4_QueueEndpointLeaksCredentialsOnUnauthGET(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	q := newBackendQueue("lmstudio-remote", 1)
	q.key = "http://svc:s3cr3t-token@10.1.2.3:1234/backend-path"
	backendQueues.Store(q.key, q)

	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/queue", nil) // deliberately unauthenticated
	rec := httptest.NewRecorder()
	s.handleQueue(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "s3cr3t-token") {
		t.Fatalf("response leaks the credential token: %s", body)
	}
	if strings.Contains(body, "svc:") || strings.Contains(body, "@") {
		t.Fatalf("response leaks URL userinfo: %s", body)
	}
	if strings.Contains(body, "backend-path") {
		t.Fatalf("response leaks the URL path: %s", body)
	}

	var resp queueSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Backends) != 1 {
		t.Fatalf("want 1 backend, got %d", len(resp.Backends))
	}
	if resp.Backends[0].Endpoint != "10.1.2.3:1234" {
		t.Errorf("Endpoint = %q; want redacted host:port %q", resp.Backends[0].Endpoint, "10.1.2.3:1234")
	}
}

// TestAdvR4_RedactQueueEndpointStripsUserinfoAndPath is the unit-level
// complement: exercises redactQueueEndpoint directly across scheme/
// userinfo/path/port permutations, plus the "never echo the raw input on
// failure" guarantee.
func TestAdvR4_RedactQueueEndpointStripsUserinfoAndPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "http://localhost:1234", "localhost:1234"},
		{"userinfo", "http://svc:token@10.1.2.3:1234", "10.1.2.3:1234"},
		{"userinfo and path", "http://svc:token@10.1.2.3:1234/v1/backend", "10.1.2.3:1234"},
		{"https no port", "https://lms.internal", "lms.internal"},
		{"empty", "", ""},
		{"unparseable", "http://[not-valid", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactQueueEndpoint(tc.in)
			if got != tc.want {
				t.Errorf("redactQueueEndpoint(%q) = %q; want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "@") || strings.Contains(got, "token") {
				t.Errorf("redactQueueEndpoint(%q) = %q leaks userinfo", tc.in, got)
			}
		})
	}
}

// ── finding 2 (MAJOR): concurrency clamp restored in makeProvider only ──────

// TestAdvR4_DeclarationRemovalCannotReturnToDefault locks in the fix for
// the round-4 MAJOR finding: removing a providers.yaml
// options.model_state.parallel declaration must let the queue fall back to
// the #555 backstop default (1), not stay pinned at the old declared
// value forever. Simulates the two states by calling newQueuedProvider
// directly the way makeProvider does: first with a declared value (4),
// then with what makeProvider now passes once the declaration is removed
// (clamped to 1, not the previous unclamped 0).
func TestAdvR4_DeclarationRemovalCannotReturnToDefault(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	const key = "http://localhost:1234"
	newQueuedProvider("lmstudio-darkstar", key, &fakeQueueProvider{name: "inner-1"}, 4)
	q, ok := backendQueues.Load(key)
	if !ok {
		t.Fatal("queue not registered after first construction")
	}
	if got := q.(*backendQueue).Snapshot().Concurrency; got != 4 {
		t.Fatalf("after declared=4: concurrency = %d; want 4", got)
	}

	// Declaration removed: makeProvider's restored clamp means it now
	// passes 1 (the backstop default), never the unclamped 0 that let the
	// round-3 repair silently no-op here.
	newQueuedProvider("lmstudio-darkstar", key, &fakeQueueProvider{name: "inner-2"}, 1)
	if got := q.(*backendQueue).Snapshot().Concurrency; got != 1 {
		t.Fatalf("after declaration removed: concurrency = %d; want 1 (the #555 backstop default)", got)
	}
}

// TestAdvR4_MakeProviderClampsUnsetParallelToOne exercises the actual
// makeProvider call site: a providers.yaml entry with no
// options.model_state.parallel key must construct its queue at
// concurrency 1, not 0 (a 0-concurrency queue never admits anyone).
func TestAdvR4_MakeProviderClampsUnsetParallelToOne(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	p, err := makeProvider("lmstudio-noparallel", ProviderConfig{
		Type:     "lmstudio",
		Endpoint: "http://localhost:19999",
	}, nil)
	if err != nil {
		t.Fatalf("makeProvider: %v", err)
	}
	qp, ok := p.(*queuedProvider)
	if !ok {
		t.Fatalf("want *queuedProvider, got %T", p)
	}
	if got := qp.queue.Snapshot().Concurrency; got != 1 {
		t.Errorf("concurrency = %d; want 1 (absent parallel key is a declaration of the backstop default)", got)
	}
}

// ── finding 3 (MINOR): canonicalizeLoopbackHost IPv6 bracket handling ───────

// TestAdvR4_CanonicalizeDoesNotMangleEndpoints verifies canonicalizeLoopbackHost
// no longer produces an unbracketed (and therefore unreachable/mis-parsed)
// IPv6 host when the default port is dropped or no port was given.
func TestAdvR4_CanonicalizeDoesNotMangleEndpoints(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"IPv6 default https port dropped", "https://[2001:db8::1]:443", "https://[2001:db8::1]"},
		{"IPv6 default http port dropped", "http://[2001:db8::1]:80", "http://[2001:db8::1]"},
		{"IPv6 no port at all", "http://[fe80::1]", "http://[fe80::1]"},
		{"IPv6 non-default port still bracketed", "http://[2001:db8::1]:8080", "http://[2001:db8::1]:8080"},
		{"loopback v4", "http://127.0.0.1:1234", "http://localhost:1234"},
		{"loopback v6 bracketed", "http://[::1]:1234", "http://localhost:1234"},
		{"LAN v4 unaffected", "http://192.168.10.191:1234", "http://192.168.10.191:1234"},
		{"remote https unaffected", "https://api.example.com", "https://api.example.com"},
		{"userinfo preserved", "http://svc:token@10.1.2.3:1234", "http://svc:token@10.1.2.3:1234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalizeLoopbackHost(tc.in)
			if got != tc.want {
				t.Errorf("canonicalizeLoopbackHost(%q) = %q; want %q", tc.in, got, tc.want)
			}
			// Whatever it produces must re-parse to the SAME host it started
			// from — the actual failure mode this bug caused was silently
			// producing a different, unreachable dial target.
			if strings.Contains(got, "2001:db8:") && !strings.Contains(got, "[2001:db8::1]") && strings.Contains(tc.in, "2001:db8::1") {
				t.Errorf("canonicalizeLoopbackHost(%q) = %q mis-splits the IPv6 literal", tc.in, got)
			}
		})
	}
}

// TestAdvR4_CanonicalizeIsIdempotent asserts canonicalizeLoopbackHost is a
// fixed point on its own output — required because buildLocalProvider
// applies host-canonicalization twice on some paths (target.BaseURL is
// already normalized by resolveLocalLLMEndpoint before local_llm.go's
// queueKey construction re-normalizes it).
func TestAdvR4_CanonicalizeIsIdempotent(t *testing.T) {
	inputs := []string{
		"https://[2001:db8::1]:443",
		"http://[2001:db8::1]:80",
		"http://[fe80::1]",
		"http://127.0.0.1:1234",
		"http://[::1]:1234",
		"http://192.168.10.191:1234",
		"https://api.example.com",
		"http://svc:token@10.1.2.3:1234/path",
	}
	for _, in := range inputs {
		once := canonicalizeLoopbackHost(in)
		twice := canonicalizeLoopbackHost(once)
		if once != twice {
			t.Errorf("canonicalizeLoopbackHost not idempotent on %q: once=%q twice=%q", in, once, twice)
		}
	}
}

// TestAdvR4_BuildLocalProviderDoesNotDoubleMangle exercises the actual
// buildLocalProvider call path end to end for an IPv6 target — the
// concrete failure buildLocalProvider's double-normalization used to
// produce before canonicalizeLoopbackHost was made idempotent.
func TestAdvR4_BuildLocalProviderDoesNotDoubleMangle(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	target := LocalLLMTarget{
		BaseURL: resolveLocalLLMEndpoint("http://[fe80::1]"),
		Backend: LocalLLMBackendOpenAICompat,
	}
	p := buildLocalProvider(target, "some-model", 0)
	qp, ok := p.(*queuedProvider)
	if !ok {
		t.Fatalf("want *queuedProvider, got %T", p)
	}
	if got := qp.queue.key; got != "http://[fe80::1]" {
		t.Errorf("queue key = %q; want %q (bracketed, single normalization)", got, "http://[fe80::1]")
	}
}

// ── finding 4 (MINOR): endpointless local-backend entries dedup ────────────

// TestAdvR4_DedupMissesEndpointlessConfigEntry verifies a providers.yaml
// entry that declares a local-backend-family type with NO explicit
// Endpoint (which resolves through the same default as the auto-discovery
// probe target) seeds configuredLocalEndpoints' dedup set, so
// autoDiscoverOpenAICompat would skip a well-known probe target instead of
// registering a second, duplicate provider name for the same physical
// backend. Operates on configuredLocalEndpoints directly (not the full
// autoDiscoverOpenAICompat, which additionally requires a reachable local
// backend to actually register anything) — matches the reviewer's
// verified shape: "endpointless entry resolves to 'http://localhost:1234';
// probe key 'http://localhost:1234'; dedup set has it? false" (pre-fix).
func TestAdvR4_DedupMissesEndpointlessConfigEntry(t *testing.T) {
	pcfg := ProvidersConfig{
		Providers: map[string]ProviderConfig{
			"lmstudio-noendpoint": {Type: "lmstudio"},
		},
	}
	configuredEndpoints, _ := configuredLocalEndpoints(pcfg)

	probeKey := normalizeLocalLLMEndpoint(openaiCompatWellKnownEndpoints[0].endpoint)
	if !configuredEndpoints[probeKey] {
		t.Errorf("configuredEndpoints[%q] = false; want true (endpointless 'lmstudio' entry resolves to the same default endpoint as the well-known probe)", probeKey)
	}
}

// TestAdvR4_DedupIgnoresEndpointlessNonLocalEntry guards the fix's
// scoping: an endpointless entry for a NON-local-backend-family type
// (e.g. "anthropic") must NOT seed configuredEndpoints with the
// openai-compat default — that would wrongly suppress auto-discovery of a
// real local LM Studio process for a config entry that has nothing to do
// with it.
func TestAdvR4_DedupIgnoresEndpointlessNonLocalEntry(t *testing.T) {
	pcfg := ProvidersConfig{
		Providers: map[string]ProviderConfig{
			"anthropic": {Type: "anthropic"},
		},
	}
	configuredEndpoints, _ := configuredLocalEndpoints(pcfg)

	probeKey := normalizeLocalLLMEndpoint(openaiCompatWellKnownEndpoints[0].endpoint)
	if configuredEndpoints[probeKey] {
		t.Errorf("configuredEndpoints[%q] = true for a non-local-backend endpointless entry ('anthropic'); this would wrongly suppress auto-discovery of a real local backend", probeKey)
	}
}

// ── finding 5 (MINOR): declared-vs-declared concurrency order-independence ─

// TestAdvR4_TwoDeclaredEntriesSameEndpointFlap verifies that two
// differently-named providers.yaml entries resolving to the same physical
// endpoint, with two different declared options.model_state.parallel
// values, converge to the SAME concurrency regardless of which entry's
// newQueuedProvider call happens to run first — Go map iteration order
// over pcfg.Providers is randomized per process, so a plain
// last-writer-wins overwrite made the outcome non-deterministic across
// otherwise-identical runs of the same config.
func TestAdvR4_TwoDeclaredEntriesSameEndpointFlap(t *testing.T) {
	const key = "http://localhost:1234"

	run := func(first, second int) int {
		resetBackendQueuesForTest()
		newQueuedProvider("entry-a", key, &fakeQueueProvider{name: "a"}, first)
		newQueuedProvider("entry-b", key, &fakeQueueProvider{name: "b"}, second)
		q, _ := backendQueues.Load(key)
		return q.(*backendQueue).Snapshot().Concurrency
	}

	forward := run(4, 1) // "entry-a" (declared 4) constructs first, "entry-b" (declared 1) second
	resetBackendQueuesForTest()
	reverse := run(1, 4) // same two declared values, opposite construction order

	if forward != reverse {
		t.Fatalf("order-dependent result: forward-order=%d reverse-order=%d for the same two declared values (4, 1)", forward, reverse)
	}
	if forward != 1 {
		t.Errorf("concurrency = %d; want 1 (min of the two declared values, 4 and 1)", forward)
	}
	resetBackendQueuesForTest()
}
