package selfupdate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// captureLog redirects the provider's operator-visible log for one test.
func captureLog(t *testing.T) *strings.Builder {
	t.Helper()
	var sb strings.Builder
	prev := logf
	logf = func(format string, args ...any) {
		sb.WriteString(strings.TrimSpace(fmt.Sprintf(format, args...)) + "\n")
	}
	t.Cleanup(func() { logf = prev })
	return &sb
}

// ─── THE MIGRATION NOTICE MUST ACTUALLY EXIST ────────────────────────────────

// Stage 2 of the rollout flips the absent-key default from warn to enforce,
// which can stop a node updating. The entire justification for staging the
// change is that operators are warned first — which is only true if something
// warns them. A SignatureModeUnset() with no production caller is a safety net
// that is documented but not installed.
func TestUnsetPostureEmitsTheMigrationNotice(t *testing.T) {
	logs := captureLog(t)
	p := New()

	cfg := defaultConfig()
	cfg.Enabled = true
	cfg.RequireSignature = SignatureWarn
	cfg.signatureKeyPresent = false // the pre-existing-config case
	cfg.CheckInterval = time.Hour

	p.noticeSignatureMigration(cfg)

	got := logs.String()
	if !strings.Contains(got, "require_signature") {
		t.Fatalf("an unset posture must be announced; got:\n%s", got)
	}
	for _, want := range []string{SignatureWarn, SignatureEnforce} {
		if !strings.Contains(got, want) {
			t.Errorf("the notice must name %q so the operator knows what changes; got:\n%s", want, got)
		}
	}
}

// An explicit choice is never nagged about.
func TestExplicitPostureEmitsNoNotice(t *testing.T) {
	logs := captureLog(t)
	p := New()

	cfg := defaultConfig()
	cfg.Enabled = true
	cfg.RequireSignature = SignatureWarn
	cfg.signatureKeyPresent = true

	p.noticeSignatureMigration(cfg)
	if s := logs.String(); s != "" {
		t.Errorf("an explicit choice must not be nagged about; got:\n%s", s)
	}
}

// The notice is throttled to the check cadence, not the (much faster) tick.
func TestMigrationNoticeIsThrottled(t *testing.T) {
	logs := captureLog(t)
	p := New()

	cfg := defaultConfig()
	cfg.Enabled = true
	cfg.RequireSignature = SignatureWarn
	cfg.signatureKeyPresent = false
	cfg.CheckInterval = time.Hour

	for i := 0; i < 5; i++ {
		p.noticeSignatureMigration(cfg)
	}
	if n := strings.Count(logs.String(), "NOTICE:"); n != 1 {
		t.Errorf("expected exactly one notice per check interval, got %d", n)
	}
}

// ─── A REFUSAL MUST REACH THE STATUS SURFACES ────────────────────────────────

// Without this, a blocked update presents as a node that is perpetually
// "updating to <tag>": ApplyPlan sets Progressing, the watchdog clears the
// in-progress flag, the next cycle re-spawns, and the refusal lives only in a
// log file in the run directory. An attack would look like a spinner.
func TestBlockedOutcomeSurfacesAsDegraded(t *testing.T) {
	root := t.TempDir()
	if err := WriteProvenanceOutcome(root, ProvenanceOutcome{
		Tag:     "v0.17.0",
		Result:  ProvenanceInvalid,
		Mode:    SignatureEnforce,
		Blocked: true,
		Message: "certificate identity does not match required",
	}); err != nil {
		t.Fatal(err)
	}

	p := New()
	p.root = root
	p.inProgress = true // as ApplyPlan would have left it

	cfg := defaultConfig()
	cfg.Enabled = true
	cfg.root = root
	cfg.RequireSignature = SignatureEnforce
	cfg.signatureKeyPresent = true

	// Prime the resolver cache so FetchLive performs no network I/O.
	primeResolver(t, p, cfg, "v0.17.0")

	live, err := p.FetchLive(context.Background(), cfg)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}

	st := p.Health()
	if st.Health != reconcile.HealthDegraded {
		t.Errorf("a refused update must read as Degraded, got %q (%s)", st.Health, st.Message)
	}
	if !strings.Contains(st.Message, "REFUSED") {
		t.Errorf("the status message must say it was refused; got %q", st.Message)
	}
	// The refusal is made visible through status, NOT by releasing the
	// dup-spawn guard. Clearing it here would let the next ~30s tick fork
	// another updater to be refused identically, forever; the watchdog's
	// ~6-minute ceiling is the correct retry cadence.
	if !p.inProgress {
		t.Error("the dup-spawn guard must be left to the watchdog, not cleared on a refusal")
	}

	// ...and it must be projected into reconcile state, not only into Health.
	state, err := p.BuildState(cfg, live, nil)
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}
	attrs := state.Resources[0].Attributes
	if attrs["require_signature"] != SignatureEnforce {
		t.Errorf("state must carry the posture, got %v", attrs["require_signature"])
	}
	lp, ok := attrs["last_provenance"].(map[string]any)
	if !ok {
		t.Fatalf("state must carry the last provenance verdict, got %v", attrs["last_provenance"])
	}
	if lp["blocked"] != true || lp["result"] != ProvenanceInvalid {
		t.Errorf("unexpected last_provenance: %v", lp)
	}
}

// A non-blocking outcome (warn mode, or success) is recorded but must not
// degrade health — otherwise every warn-mode node would look broken.
func TestNonBlockingOutcomeDoesNotDegrade(t *testing.T) {
	root := t.TempDir()
	if err := WriteProvenanceOutcome(root, ProvenanceOutcome{
		Tag:     "v0.17.0",
		Result:  ProvenanceOK,
		Mode:    SignatureWarn,
		Blocked: false,
		Message: "identity=...",
	}); err != nil {
		t.Fatal(err)
	}

	p := New()
	p.root = root
	cfg := defaultConfig()
	cfg.Enabled = true
	cfg.root = root
	cfg.signatureKeyPresent = true
	primeResolver(t, p, cfg, "v0.17.0")

	if _, err := p.FetchLive(context.Background(), cfg); err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	if st := p.Health(); st.Health == reconcile.HealthDegraded {
		t.Errorf("a successful verification must not degrade health; got %q", st.Message)
	}
}

// An absent outcome file is the normal state before the first update and must
// be inert.
func TestAbsentOutcomeIsInert(t *testing.T) {
	if o := ReadProvenanceOutcome(t.TempDir()); o != nil {
		t.Errorf("no outcome file should read as nil, got %+v", o)
	}
	if o := ReadProvenanceOutcome(""); o != nil {
		t.Errorf("empty root should read as nil, got %+v", o)
	}
}

// primeResolver seeds the throttled release cache so FetchLive resolves without
// touching the network.
func primeResolver(t *testing.T, p *Provider, cfg *SelfUpdateConfig, tag string) {
	t.Helper()
	base := "https://github.com/" + cfg.Repo + "/releases/download/" + tag
	p.resolver.mu.Lock()
	p.resolver.cached = &resolvedRelease{
		Tag:            tag,
		AssetName:      AssetName(),
		AssetURL:       base + "/" + AssetName(),
		ChecksumURL:    base + "/checksums.txt",
		SignatureURL:   base + "/checksums.txt.sig",
		CertificateURL: base + "/checksums.txt.pem",
	}
	p.resolver.cachedAt = time.Now()
	p.resolver.key = cacheKey{repo: cfg.Repo, channel: cfg.Channel, pin: cfg.Pin}
	p.resolver.mu.Unlock()
}

// A refusal must not stick once it no longer applies — otherwise relaxing the
// posture, or cutting a corrected release, would leave the node reading as
// Degraded forever.
func TestBlockedStatusClearsWhenTheVerdictNoLongerApplies(t *testing.T) {
	root := t.TempDir()
	if err := WriteProvenanceOutcome(root, ProvenanceOutcome{
		Tag: "v0.17.0", Result: ProvenanceInvalid, Blocked: true, Message: "bad identity",
	}); err != nil {
		t.Fatal(err)
	}

	p := New()
	p.root = root
	cfg := defaultConfig()
	cfg.Enabled = true
	cfg.root = root
	cfg.signatureKeyPresent = true

	primeResolver(t, p, cfg, "v0.17.0")
	if _, err := p.FetchLive(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if p.Health().Health != reconcile.HealthDegraded {
		t.Fatal("precondition: the refusal should degrade health")
	}

	// A corrected release is cut under a new tag; the stale verdict is for the
	// old one and must no longer degrade.
	primeResolver(t, p, cfg, "v0.17.1")
	if _, err := p.FetchLive(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if st := p.Health(); st.Health == reconcile.HealthDegraded {
		t.Errorf("a verdict for a superseded tag must not keep the node degraded; got %q", st.Message)
	}
}
