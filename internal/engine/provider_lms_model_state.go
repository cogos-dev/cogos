// provider_lms_model_state.go — declarative reconciler for LM Studio model/context state.
//
// LMSModelStateProvider implements reconcile.Reconcilable (health + lifecycle)
// only — it is NOT an engine.Provider. Dispatch to an LM Studio backend already
// happens through the OpenAICompatProvider (type "lmstudio"); this provider adds
// a second, orthogonal concern on top of the same backend: keeping the *right
// model loaded at the right context length*.
//
// Architecture (mirrors provider_mlx_supervised.go, swapping the actuator and
// the reconciled dimension)
//
//   - Config is declared in providers(.local).yaml under a dispatch backend's
//     options.model_state block. The reconciler is OPT-IN: it activates only
//     when options.model_state.manage == true. No model_state block ⇒ Suspended.
//   - The reconciled dimension is "is <model> loaded at <context_length>?", read
//     from GET {baseURL}/api/v0/models (LM Studio's native REST surface, which
//     unlike /v1/models exposes per-model `state` and `loaded_context_length`).
//   - The actuator is the Node @lmstudio/sdk websocket bridge at
//     scripts/lms-actuator/ (load / unload / set-context over ws://). On a
//     localhost backend a `~/.lmstudio/bin/lms` fast-path is permitted; remote
//     backends (Eclipse, off-LAN) always use the SDK actuator because the lms
//     CLI cannot reach a remote instance (LM Link gated).
//   - Health() is O(1): it reads the last cached /api/v0/models rows (populated
//     by FetchLive) under a RWMutex and never performs I/O. The autonomic ticker
//     calls Health() on every self-heal tick, so it MUST stay non-blocking.
//   - FetchLive is where the network probe lives (~4s timeout). An unreachable
//     backend maps to Suspended/Unknown (NOT Degraded) so the self-heal driver
//     does not try to "fix" a machine that is simply off or off-LAN.
//
// TWO HARD GUARDRAILS honored by this file:
//  1. NON-DESTRUCTIVE. FetchLive / Health / ComputePlan are read-only probes.
//     ApplyPlan is the ONLY method that mutates model state, and it does so via
//     an external actuator process; the actuator is invoked with the token in
//     the environment (never argv). This file never issues a load/unload itself.
//  2. OPT-IN, DISABLED BY DEFAULT. Without options.model_state.manage == true the
//     provider reports Suspended and produces an empty plan — a kernel restart
//     reconciles nothing until the operator opts in.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// lmsModelStateType is the reconcile type identifier for this provider.
const lmsModelStateType = "lms-model-state"

// lmsFetchTimeout bounds the /api/v0/models probe in FetchLive.
const lmsFetchTimeout = 4 * time.Second

// lmsApplyTimeout bounds a single actuator invocation in ApplyPlan.
const lmsApplyTimeout = 180 * time.Second

// lmsPsProbeTimeout bounds the `lms ps --json` parallel probe in FetchLive.
// This is deliberately its own (short) budget rather than sharing lmsFetchTimeout
// — a hung lms CLI must not eat the whole /api/v0/models probe window.
const lmsPsProbeTimeout = 3 * time.Second

// lmsActuatorTokenEnv is the environment variable the Node actuator reads for
// its Bearer token. ApplyPlan sets it from the provider's cached token; it is
// never passed on argv.
const lmsActuatorTokenEnv = "LMS_ACTUATOR_TOKEN"

// ── config ─────────────────────────────────────────────────────────────────────

// lmsModelStateConfig is the parsed options.model_state block for one backend.
type lmsModelStateConfig struct {
	Manage        bool   // opt-in switch; false ⇒ Suspended, empty plan
	Model         string // target model id that should be loaded
	ContextLength int    // desired loaded_context_length (0 ⇒ don't manage context)
	Parallel      int    // watched for drift, never actuated (LM Studio SDK load config has no per-load parallelism knob; parallelism is a server/JIT setting). Observed only on local backends via `lms ps --json` — see probeParallelLocal.
	KeepWarm      bool   // hint: keep loaded even when idle (advisory metadata)
	JITEvict      bool   // if true, unload a non-target model that crowds the card
}

// ── live state ───────────────────────────────────────────────────────────────

// lmsModelRow is one entry from GET /api/v0/models. Not-loaded models omit
// loaded_context_length entirely, so it is a pointer: a missing/null field
// decodes to nil rather than shadowing a real value with 0.
type lmsModelRow struct {
	ID                  string `json:"id"`
	State               string `json:"state"` // "loaded" | "not-loaded" | "loading"
	LoadedContextLength *int   `json:"loaded_context_length,omitempty"`
	MaxContextLength    int    `json:"max_context_length,omitempty"`
	Type                string `json:"type,omitempty"`

	// Parallel is NOT part of the /api/v0/models response — LM Studio does not
	// expose it there (confirmed live against Darkstar's :1234). It is merged in
	// after the fact, on local backends only, from `lms ps --json` (see
	// probeParallelLocal). nil ⇒ unobserved (remote backend, or the CLI probe
	// failed) — never treated as a mismatch; distinct from an observed 0.
	Parallel *int `json:"-"`
}

// lmsPsRow is one entry from `lms ps --json` (the local-only lms CLI, no --host
// flag — same LM-Link-gated local/remote asymmetry the actuator's fast-path
// already documents). Confirmed live shape:
// {"identifier":"ornith-1.0-35b",...,"parallel":1}.
type lmsPsRow struct {
	Identifier string `json:"identifier"`
	ModelKey   string `json:"modelKey"`
	Parallel   int    `json:"parallel"`
}

// lmsModelsResponse is the /api/v0/models envelope.
type lmsModelsResponse struct {
	Data []lmsModelRow `json:"data"`
}

// ── provider ───────────────────────────────────────────────────────────────────

// LMSModelStateProvider reconciles the loaded-model/context state of a single
// LM Studio backend against a declared options.model_state target.
type LMSModelStateProvider struct {
	// --- identity / target ---
	name    string // provider name as declared in providers.yaml
	baseURL string // e.g. "http://192.168.10.191:1234" (REST base, no path)
	wsURL   string // e.g. "ws://192.168.10.191:1234" (SDK bridge)
	host    string // bare host, e.g. "192.168.10.191"
	port    int    // e.g. 1234
	local   bool   // endpoint is localhost/127.0.0.1 ⇒ lms CLI fast-path allowed

	// --- auth ---
	token string // Bearer token; from the backend's api_key_env (may be empty for localhost)

	// --- desired state ---
	target lmsModelStateConfig

	// --- actuator ---
	actuatorScript string // absolute path to scripts/lms-actuator/lms-actuator.mjs
	nodeBin        string // node binary (default "node")
	lmsCLI         string // ~/.lmstudio/bin/lms (local fast-path)

	// --- cached live state (Health reads this without I/O) ---
	mu         sync.RWMutex
	lastLive   []lmsModelRow
	lastProbed time.Time
	lastErr    error // last FetchLive error (unreachable ⇒ Suspended)

	// --- reconcile metadata ---
	workspaceRoot string
}

// newLMSModelStateProvider constructs the provider from a dispatch ProviderConfig.
// The caller (BuildRouter) has already confirmed options.model_state.manage.
func newLMSModelStateProvider(name string, cfg ProviderConfig, token, workspaceRoot string) (*LMSModelStateProvider, error) {
	target := parseModelStateOptions(cfg.Options)

	baseURL := strings.TrimRight(cfg.Endpoint, "/")
	if baseURL == "" {
		baseURL = "http://localhost:1234"
	}
	host, port := hostPort(baseURL, 1234)
	local := isLocalHost(host)

	// ws:// bridge shares host:port with the REST endpoint.
	wsURL := fmt.Sprintf("ws://%s:%d", host, port)

	// Resolve the actuator script relative to the workspace root when possible;
	// fall back to a repo-relative path. ApplyPlan re-checks existence and maps
	// an absent actuator to Suspended, so a wrong guess here is non-fatal.
	actuator := resolveActuatorScript(workspaceRoot)

	// Local lms CLI fast-path binary.
	lmsCLI := ""
	if home, err := homeDir(); err == nil {
		lmsCLI = filepath.Join(home, ".lmstudio", "bin", "lms")
	}

	return &LMSModelStateProvider{
		name:           name,
		baseURL:        baseURL,
		wsURL:          wsURL,
		host:           host,
		port:           port,
		local:          local,
		token:          token,
		target:         target,
		actuatorScript: actuator,
		nodeBin:        "node",
		lmsCLI:         lmsCLI,
		workspaceRoot:  workspaceRoot,
	}, nil
}

// parseModelStateOptions extracts the model_state sub-map from a provider's
// Options. A missing block yields a zero config (Manage=false ⇒ Suspended).
func parseModelStateOptions(opts map[string]interface{}) lmsModelStateConfig {
	var c lmsModelStateConfig
	if opts == nil {
		return c
	}
	raw, ok := opts["model_state"]
	if !ok {
		return c
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		// yaml.v3 may decode nested maps as map[interface{}]interface{}.
		if mi, ok2 := raw.(map[interface{}]interface{}); ok2 {
			m = make(map[string]interface{}, len(mi))
			for k, v := range mi {
				m[fmt.Sprint(k)] = v
			}
		} else {
			return c
		}
	}
	c.Manage = optBool(m, "manage")
	c.Model = optStr(m, "model")
	c.ContextLength = optInt(m, "context_length")
	c.Parallel = optInt(m, "parallel")
	c.KeepWarm = optBool(m, "keep_warm")
	c.JITEvict = optBool(m, "jit_evict")
	return c
}

// ── reconcile.Reconcilable interface ──────────────────────────────────────────

// Type returns the reconcilable type identifier.
func (p *LMSModelStateProvider) Type() string { return lmsModelStateType }

// SetToken satisfies reconcile.Tokenable. BuildRouter also injects the token at
// construction (via the backend's api_key_env), but the framework auto-wire path
// from {TYPE}_TOKEN uses this.
func (p *LMSModelStateProvider) SetToken(token string) {
	p.mu.Lock()
	p.token = token
	p.mu.Unlock()
}

// LoadConfig records the workspace root and (re)resolves the actuator script
// path now that a concrete root is known. Config is already parsed at
// construction from ProviderConfig; the daemon stub path drives full parsing.
func (p *LMSModelStateProvider) LoadConfig(root string) (any, error) {
	p.mu.Lock()
	p.workspaceRoot = root
	if root != "" {
		if s := resolveActuatorScript(root); s != "" {
			p.actuatorScript = s
		}
	}
	p.mu.Unlock()
	return &p.target, nil
}

// FetchLive probes GET {baseURL}/api/v0/models with the Bearer token and caches
// the decoded rows. This is the ONLY network I/O in the read path; Health reads
// the cache it populates. An unreachable backend is cached as lastErr so Health
// can map it to Suspended rather than Degraded.
func (p *LMSModelStateProvider) FetchLive(ctx context.Context, _ any) (any, error) {
	rows, err := p.probeModels(ctx)

	// Merge the local-only `lms ps --json` parallel probe. A probe failure here
	// (binary missing, non-zero exit, bad JSON) is NON-FATAL — it must not fail
	// the whole FetchLive or affect Health beyond leaving Parallel nil
	// (unobserved, not wrong), mirroring how a missing loaded_context_length is
	// treated as unknown rather than a mismatch.
	if err == nil && p.local {
		if parallel, perr := p.probeParallelLocal(ctx); perr == nil {
			mergeParallel(rows, parallel)
		}
	}

	p.mu.Lock()
	p.lastProbed = time.Now()
	p.lastErr = err
	if err == nil {
		p.lastLive = rows
	}
	p.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return rows, nil
}

// probeModels performs the read-only GET /api/v0/models request.
func (p *LMSModelStateProvider) probeModels(ctx context.Context) ([]lmsModelRow, error) {
	reqCtx, cancel := context.WithTimeout(ctx, lmsFetchTimeout)
	defer cancel()

	url := p.baseURL + "/api/v0/models"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("lms-model-state %q: build request: %w", p.name, err)
	}
	p.mu.RLock()
	token := p.token // mutated by SetToken — read under the lock
	p.mu.RUnlock()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: lmsFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lms-model-state %q: unreachable: %w", p.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lms-model-state %q: /api/v0/models returned %d", p.name, resp.StatusCode)
	}

	var out lmsModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("lms-model-state %q: decode /api/v0/models: %w", p.name, err)
	}
	return out.Data, nil
}

// probeParallelLocal shells out to `lms ps --json` (the local-only lms CLI —
// confirmed no --host flag, so it cannot reach a remote instance; the same
// LM-Link-gated asymmetry the actuator's fast-path already documents) and
// returns a map of model identifier -> observed parallel value. Callers must
// gate this behind p.local; it is only meaningful against the local instance.
func (p *LMSModelStateProvider) probeParallelLocal(ctx context.Context) (map[string]int, error) {
	if !p.local || p.lmsCLI == "" || !statOK(p.lmsCLI) {
		return nil, fmt.Errorf("lms-model-state %q: lms CLI fast-path unavailable for parallel probe", p.name)
	}

	psCtx, cancel := context.WithTimeout(ctx, lmsPsProbeTimeout)
	defer cancel()

	out, err := exec.CommandContext(psCtx, p.lmsCLI, "ps", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("lms-model-state %q: lms ps --json: %w", p.name, err)
	}

	var rows []lmsPsRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("lms-model-state %q: decode lms ps --json: %w", p.name, err)
	}

	result := make(map[string]int, len(rows))
	for _, r := range rows {
		id := r.Identifier
		if id == "" {
			id = r.ModelKey
		}
		if id == "" {
			continue
		}
		result[id] = r.Parallel
	}
	return result, nil
}

// mergeParallel copies observed parallel values from an `lms ps --json` probe
// into the matching /api/v0/models rows, using the same prefix-either-direction
// identifier matching as findModelRow (quant suffixes, publisher prefixes).
// Rows with no match are left with a nil Parallel (unobserved).
func mergeParallel(rows []lmsModelRow, parallel map[string]int) {
	for i := range rows {
		for id, val := range parallel {
			if modelIDMatch(rows[i].ID, id) {
				v := val
				rows[i].Parallel = &v
				break
			}
		}
	}
}

// ComputePlan diffs the declared target against the live rows and emits
// Update-typed actions distinguished by a Name suffix:
//
//	<name>/load    — target model not loaded
//	<name>/context — loaded but at the wrong context (unload+reload; no live resize)
//	<name>/unload  — jit_evict only: a non-target model is loaded and crowds the card
//
// An empty plan (Synced) means: the target row exists, state=="loaded", and
// loaded_context_length matches the target (or no context is being managed).
func (p *LMSModelStateProvider) ComputePlan(config any, live any, _ *reconcile.State) (*reconcile.Plan, error) {
	target := p.resolveTarget(config)
	plan := &reconcile.Plan{ResourceType: lmsModelStateType}

	// Opt-out: no management requested ⇒ nothing to do.
	if !target.Manage {
		return plan, nil
	}

	rows := rowsFrom(live, p)

	// Find the target model row, preferring a state=="loaded" match so a
	// not-loaded duplicate row cannot shadow the real loaded one.
	targetRow := findModelRow(rows, target.Model)

	// Never disturb an in-flight load. While state=="loading" the loaded context
	// is unknown (nil loaded_context_length), which must NOT be read as a
	// wrong-context mismatch. BOTH callers of ComputePlan reach here — the
	// autonomic ticker (which gates on Health and skips Progressing) AND the
	// always-on ReconcileDaemon (which does not gate on Health) — so this guard
	// in ComputePlan is the single, caller-independent defense against the
	// load-interrupt race: return an empty plan and let the load finish. Health()
	// reports Progressing separately. Loads can exceed the daemon poll interval
	// (e.g. 262144 ≈ multi-minute), so interrupting them would prevent the model
	// from ever finishing loading.
	if targetRow != nil && targetRow.State == "loading" {
		return plan, nil
	}

	// jit_evict FIRST: any eviction must precede the load so VRAM is actually
	// freed before we load the target onto the card (otherwise ApplyPlan would
	// load onto a still-crowded card and fail, deferring convergence a cycle).
	// Only when explicitly enabled — this is destructive-adjacent.
	if target.JITEvict {
		for i := range rows {
			r := &rows[i]
			if r.State == "loaded" && r.Type != "embeddings" && !modelIDMatch(r.ID, target.Model) {
				plan.Actions = append(plan.Actions, reconcile.Action{
					Action:       reconcile.ActionUpdate,
					ResourceType: lmsModelStateType,
					Name:         p.name + "/unload",
					Details: map[string]any{
						"model":  r.ID,
						"reason": "jit_evict — non-target model loaded, crowds the card",
					},
				})
				plan.Summary.Updates++
			}
		}
	}

	switch {
	case targetRow == nil || targetRow.State == "not-loaded":
		plan.Actions = append(plan.Actions, reconcile.Action{
			Action:       reconcile.ActionUpdate,
			ResourceType: lmsModelStateType,
			Name:         p.name + "/load",
			Details: map[string]any{
				"model":          target.Model,
				"context_length": target.ContextLength,
				"reason":         "target model not loaded",
			},
		})
		plan.Summary.Updates++

	case target.ContextLength > 0 && contextMismatch(targetRow, target.ContextLength):
		plan.Actions = append(plan.Actions, reconcile.Action{
			Action:       reconcile.ActionUpdate,
			ResourceType: lmsModelStateType,
			Name:         p.name + "/context",
			Details: map[string]any{
				"model":          target.Model,
				"context_length": target.ContextLength,
				"loaded_context": ctxStr(targetRow.LoadedContextLength),
				"reason":         "loaded at wrong context — LM Studio has no live resize; unload+reload",
			},
		})
		plan.Summary.Updates++
	}

	return plan, nil
}

// ApplyPlan executes each action via the Node SDK actuator (token in ENV, not
// argv). On a localhost backend the lms CLI fast-path is used when present.
//
// GUARDRAIL 1: this is the only mutating method. In tests the actuator binary is
// replaced with a fake so no real load/unload reaches a live backend.
func (p *LMSModelStateProvider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	var results []reconcile.Result

	for _, action := range plan.Actions {
		var (
			op    string
			model string
		)
		switch {
		case strings.HasSuffix(action.Name, "/load"):
			op, model = "load", detailStr(action.Details, "model")
		case strings.HasSuffix(action.Name, "/context"):
			// No live resize: a context change is an unload+reload. The actuator
			// performs both under the "set-context" verb.
			op, model = "set-context", detailStr(action.Details, "model")
		case strings.HasSuffix(action.Name, "/unload"):
			op, model = "unload", detailStr(action.Details, "model")
		default:
			results = append(results, reconcile.Result{
				Phase: "apply", Action: string(action.Action), Name: action.Name,
				Status: reconcile.ApplySkipped, Error: "unrecognized action suffix",
			})
			continue
		}

		ctxLen := detailInt(action.Details, "context_length")

		if err := p.invokeActuator(ctx, op, model, ctxLen); err != nil {
			results = append(results, reconcile.Result{
				Phase: "apply", Action: string(action.Action), Name: action.Name,
				Status: reconcile.ApplyFailed, Error: err.Error(),
			})
			// Non-fatal: surface and continue with remaining actions.
			continue
		}
		results = append(results, reconcile.Result{
			Phase: "apply", Action: string(action.Action), Name: action.Name,
			Status: reconcile.ApplySucceeded,
		})
	}
	return results, nil
}

// invokeActuator runs the load/unload/set-context operation. It builds the
// command (SDK actuator by default; lms CLI on a local backend) with the token
// injected via the environment, never argv.
func (p *LMSModelStateProvider) invokeActuator(ctx context.Context, op, model string, ctxLen int) error {
	if model == "" {
		return fmt.Errorf("lms-model-state %q: %s: empty model id", p.name, op)
	}

	applyCtx, cancel := context.WithTimeout(ctx, lmsApplyTimeout)
	defer cancel()

	cmd, emitsJSON, err := p.buildActuatorCmd(applyCtx, op, model, ctxLen)
	if err != nil {
		return err
	}

	// Token via ENV, never argv. Read under RLock (SetToken mutates it).
	p.mu.RLock()
	token := p.token
	p.mu.RUnlock()
	cmd.Env = append(os.Environ(), lmsActuatorTokenEnv+"="+token)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("lms-model-state %q: actuator %s failed: %w (stderr: %s)",
			p.name, op, err, strings.TrimSpace(stderr.String()))
	}
	// Defense-in-depth: even on exit 0, honour the SDK actuator's {"ok":...}
	// contract so an ok==false result printed with a zero exit (a dropped-await
	// JS footgun) is not mistaken for a successful heal. The lms CLI fast-path
	// prints plain text and is exempt (emitsJSON == false).
	if emitsJSON {
		if perr := parseActuatorResult(stdout.String()); perr != nil {
			return fmt.Errorf("lms-model-state %q: actuator %s: %w", p.name, op, perr)
		}
	}
	return nil
}

// buildActuatorCmd assembles the exec.Cmd against an already-timed context.
// SDK actuator by default; on a local backend the lms CLI fast-path is used when
// the binary exists. The caller owns the context's cancel.
// The returned emitsJSON is true only for the SDK actuator path, whose contract
// is to print a final {"ok":...} result line; the lms CLI fast-path prints plain
// human text, so its output must not be JSON-parsed.
func (p *LMSModelStateProvider) buildActuatorCmd(ctx context.Context, op, model string, ctxLen int) (cmd *exec.Cmd, emitsJSON bool, err error) {
	// Local fast-path: lms CLI (localhost only). Plain-text output, not JSON.
	if p.local && p.lmsCLI != "" && statOK(p.lmsCLI) {
		if op == "unload" {
			return exec.CommandContext(ctx, p.lmsCLI, "unload", model), false, nil
		}
		args := []string{"load", model}
		if ctxLen > 0 {
			args = append(args, "--context-length", fmt.Sprintf("%d", ctxLen))
		}
		return exec.CommandContext(ctx, p.lmsCLI, args...), false, nil
	}

	// SDK actuator (remote, or local without the CLI). Requires the script.
	p.mu.RLock()
	actuatorScript := p.actuatorScript // mutated by LoadConfig — read under the lock
	p.mu.RUnlock()
	if actuatorScript == "" {
		return nil, false, fmt.Errorf("lms-model-state %q: SDK actuator script not resolved", p.name)
	}
	if _, statErr := os.Stat(actuatorScript); statErr != nil {
		return nil, false, fmt.Errorf("lms-model-state %q: SDK actuator not installed at %s (run: cd %s && npm install)",
			p.name, actuatorScript, filepath.Dir(actuatorScript))
	}

	args := []string{
		actuatorScript, op,
		"--host", p.host,
		"--port", fmt.Sprintf("%d", p.port),
		"--model", model,
	}
	if ctxLen > 0 {
		args = append(args, "--context-length", fmt.Sprintf("%d", ctxLen))
	}
	// NOTE: `parallel` is intentionally NOT threaded to the actuator. LM Studio's
	// @lmstudio/sdk load config (v1.5.0) has no per-load parallelism knob —
	// parallelism is a server/JIT setting, not a load-config field — so passing it
	// would be a silent no-op. It remains a declared model_state field only for
	// state reporting (BuildState attributes), not for actuation.
	return exec.CommandContext(ctx, p.nodeBin, args...), true, nil
}

// actuatorResult is the JSON contract emitted on the SDK actuator's final stdout
// line. Success is reported ONLY when the child exits 0 AND ok==true.
type actuatorResult struct {
	Ok    bool   `json:"ok"`
	Error string `json:"error"`
}

// parseActuatorResult validates the SDK actuator's stdout. Defense-in-depth
// against a future actuator change that prints {"ok":false} while exiting 0
// (a dropped await / swallowed catch): treat ok==false — or output with no
// parseable result line at all — as a failure even on a zero exit code.
func parseActuatorResult(stdout string) error {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var res actuatorResult
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			continue // not the JSON result line; keep scanning upward
		}
		if !res.Ok {
			if res.Error != "" {
				return fmt.Errorf("actuator reported failure: %s", res.Error)
			}
			return fmt.Errorf("actuator reported ok=false")
		}
		return nil
	}
	return fmt.Errorf("actuator produced no parseable {\"ok\":...} result line")
}

// BuildState projects one Resource for this backend describing the loaded model.
func (p *LMSModelStateProvider) BuildState(_ any, live any, existing *reconcile.State) (*reconcile.State, error) {
	state := reconcile.NewState(lmsModelStateType)
	if existing != nil {
		state = existing
	}

	rows := rowsFrom(live, p)
	loaded := firstLoaded(rows)

	attrs := map[string]any{
		"model":          p.target.Model,
		"parallel":       p.target.Parallel,
		"keep_warm":      p.target.KeepWarm,
		"target_context": p.target.ContextLength,
	}
	externalID := ""
	if loaded != nil {
		externalID = loaded.ID
		attrs["loaded_model"] = loaded.ID
		attrs["loaded_context_length"] = ctxStr(loaded.LoadedContextLength)
		attrs["max_context_length"] = loaded.MaxContextLength
		attrs["observed_parallel"] = parallelStr(loaded.Parallel)
	}

	state.Resources = []reconcile.Resource{{
		Address:    lmsModelStateType + "/" + p.name,
		Type:       lmsModelStateType,
		Mode:       reconcile.ModeManaged,
		ExternalID: externalID,
		Name:       p.name,
		Attributes: attrs,
	}}
	return state, nil
}

// Health returns the three-axis status from cached rows, WITHOUT blocking.
//
//	no model_state.manage  ⇒ Suspended/Unknown/Idle  (opt-in, not a failure)
//	actuator not installed ⇒ Suspended/Unknown/Idle  (clear install message)
//	unreachable / off-LAN  ⇒ Suspended/Unknown/Idle  (do NOT self-heal)
//	state=="loading"       ⇒ Progressing/Unknown/Waiting
//	target absent          ⇒ Missing/OutOfSync/Idle
//	wrong model or context ⇒ Degraded/OutOfSync/Idle
//	loaded at target ctx   ⇒ Healthy/Synced/Idle
func (p *LMSModelStateProvider) Health() reconcile.ResourceStatus {
	p.mu.RLock()
	rows := p.lastLive
	probed := p.lastProbed
	lastErr := p.lastErr
	target := p.target
	actuatorScript := p.actuatorScript // mutated by LoadConfig — read under the lock
	p.mu.RUnlock()

	// Opt-in gate: not managing ⇒ Suspended.
	if !target.Manage {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthSuspended,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("lms-model-state %q: options.model_state.manage not set — opt-in feature", p.name),
		}
	}

	// Actuator presence check (stat is fast, O(1)). A missing actuator means we
	// could probe but never remediate — surface as Suspended with a fix hint.
	if actuatorScript == "" {
		return p.suspended("SDK actuator script path unresolved")
	}
	if _, err := os.Stat(actuatorScript); err != nil {
		// A local backend with the lms CLI available can still remediate.
		if !(p.local && p.lmsCLI != "" && statOK(p.lmsCLI)) {
			return p.suspended(fmt.Sprintf("SDK actuator not installed at %s (cd %s && npm install)",
				actuatorScript, filepath.Dir(actuatorScript)))
		}
	}

	// Not yet probed.
	if probed.IsZero() {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationWaiting,
			Message:   fmt.Sprintf("lms-model-state %q: not yet probed (%s)", p.name, p.baseURL),
		}
	}

	// Unreachable / off-LAN ⇒ Suspended (NOT Degraded): don't self-heal a box
	// that is simply off or unreachable.
	if lastErr != nil {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthSuspended,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("lms-model-state %q: backend unreachable — %v", p.name, lastErr),
		}
	}

	targetRow := findModelRow(rows, target.Model)

	// Loading in progress.
	if targetRow != nil && targetRow.State == "loading" {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationWaiting,
			Message:   fmt.Sprintf("lms-model-state %q: %s loading", p.name, target.Model),
		}
	}

	// Target absent (or not loaded) ⇒ Missing.
	if targetRow == nil || targetRow.State == "not-loaded" {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("lms-model-state %q: target model %q not loaded", p.name, target.Model),
		}
	}

	// Wrong context ⇒ Degraded/OutOfSync.
	if target.ContextLength > 0 && contextMismatch(targetRow, target.ContextLength) {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message: fmt.Sprintf("lms-model-state %q: %s loaded at context %s, want %d%s",
				p.name, target.Model, ctxStr(targetRow.LoadedContextLength), target.ContextLength,
				parallelGapNote(target, p.local)),
		}
	}

	// Wrong parallel (local only — alarm-only, never actuated: LM Studio's load
	// config has no per-load parallelism knob) ⇒ Degraded/OutOfSync.
	if target.Parallel > 0 && p.local && parallelMismatch(targetRow, target.Parallel) {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message: fmt.Sprintf("lms-model-state %q: %s loaded with parallel %s, want %d",
				p.name, target.Model, parallelStr(targetRow.Parallel), target.Parallel),
		}
	}

	// Loaded at target ⇒ Healthy/Synced.
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusSynced,
		Health:    reconcile.HealthHealthy,
		Operation: reconcile.OperationIdle,
		Message: fmt.Sprintf("lms-model-state %q: %s loaded at context %s%s",
			p.name, target.Model, ctxStr(targetRow.LoadedContextLength), parallelGapNote(target, p.local)),
	}
}

func (p *LMSModelStateProvider) suspended(msg string) reconcile.ResourceStatus {
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusUnknown,
		Health:    reconcile.HealthSuspended,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("lms-model-state %q: %s", p.name, msg),
	}
}

// resolveTarget prefers a config passed through LoadConfig/ComputePlan, falling
// back to the provider's own parsed target.
func (p *LMSModelStateProvider) resolveTarget(config any) lmsModelStateConfig {
	if c, ok := config.(*lmsModelStateConfig); ok && c != nil {
		return *c
	}
	if c, ok := config.(lmsModelStateConfig); ok {
		return c
	}
	return p.target
}

// ── row helpers ────────────────────────────────────────────────────────────────

// rowsFrom returns the live rows from a FetchLive result, falling back to the
// provider's cache when live is nil (e.g. daemon-driven cycle).
func rowsFrom(live any, p *LMSModelStateProvider) []lmsModelRow {
	if rows, ok := live.([]lmsModelRow); ok {
		return rows
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastLive
}

// findModelRow returns the row matching model, preferring a loaded row so a
// not-loaded duplicate cannot shadow the real one. Returns nil if absent.
func findModelRow(rows []lmsModelRow, model string) *lmsModelRow {
	var fallback *lmsModelRow
	for i := range rows {
		r := &rows[i]
		if !modelIDMatch(r.ID, model) {
			continue
		}
		if r.State == "loaded" || r.State == "loading" {
			return r
		}
		if fallback == nil {
			fallback = r
		}
	}
	return fallback
}

// firstLoaded returns the first loaded non-embedding row, or nil.
func firstLoaded(rows []lmsModelRow) *lmsModelRow {
	for i := range rows {
		if rows[i].State == "loaded" && rows[i].Type != "embeddings" {
			return &rows[i]
		}
	}
	return nil
}

// modelIDMatch matches an LM Studio model id against a configured id, allowing
// prefix matches in either direction (quant suffixes, publisher prefixes).
func modelIDMatch(rowID, want string) bool {
	if want == "" {
		return false
	}
	return rowID == want || strings.HasPrefix(rowID, want) || strings.HasPrefix(want, rowID)
}

// contextMismatch reports whether a loaded row's context differs from target.
// A nil loaded_context_length (not-loaded/absent) is treated as a mismatch when
// a target is set — but callers gate this behind a state=="loaded" check.
func contextMismatch(r *lmsModelRow, target int) bool {
	// Unknown loaded context (nil during loading / not-loaded, or a nil row) is
	// NOT a mismatch: we cannot compare, and treating it as one would trigger a
	// disruptive unload+reload against an in-flight load. Callers gate on
	// state=="loading"/"not-loaded" before reaching here; this is defense-in-depth.
	if r == nil || r.LoadedContextLength == nil {
		return false
	}
	return *r.LoadedContextLength != target
}

// ctxStr renders a *int context length ("null" when nil).
func ctxStr(v *int) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *v)
}

// parallelMismatch reports whether a loaded row's observed parallel value
// differs from target. A nil Parallel (unobserved — remote backend, or the
// `lms ps --json` probe failed/found no match) is NOT a mismatch: we cannot
// compare, and this dimension is alarm-only anyway (never actuated).
func parallelMismatch(r *lmsModelRow, target int) bool {
	if r == nil || r.Parallel == nil {
		return false
	}
	return *r.Parallel != target
}

// parallelStr renders a *int observed-parallel value ("null" when nil).
func parallelStr(v *int) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *v)
}

// parallelGapNote returns an informational suffix for Health() messages when a
// parallel target is declared but the backend is remote: `lms ps --json` has no
// --host flag, so remote backends cannot be probed for parallel drift through
// this mechanism. The gap must stay visible rather than presenting as full
// coverage when only context is actually being watched.
func parallelGapNote(target lmsModelStateConfig, local bool) string {
	if target.Parallel > 0 && !local {
		return "; parallel target declared but backend is remote — not observable via lms ps"
	}
	return ""
}

// ── option / detail helpers ────────────────────────────────────────────────────

func optBool(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "yes" || v == "1"
	}
	return false
}

func optInt(m map[string]interface{}, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func detailStr(d map[string]any, key string) string {
	if d == nil {
		return ""
	}
	s, _ := d[key].(string)
	return s
}

func detailInt(d map[string]any, key string) int {
	if d == nil {
		return 0
	}
	switch v := d[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// ── url / host helpers ─────────────────────────────────────────────────────────

// hostPort splits "http://host:port" into (host, port), defaulting the port.
func hostPort(endpoint string, defaultPort int) (string, int) {
	s := endpoint
	if idx := strings.Index(s, "://"); idx >= 0 {
		s = s[idx+3:]
	}
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	host := s
	port := defaultPort
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		host = s[:idx]
		port = extractPort(endpoint, defaultPort)
	}
	if host == "" {
		host = "localhost"
	}
	return host, port
}

// isLocalHost reports whether host is a loopback name/address.
func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// statOK reports whether a path exists.
func statOK(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// resolveActuatorScript computes the absolute path to the Node actuator script.
// It prefers <workspaceRoot>/scripts/lms-actuator/lms-actuator.mjs; if that does
// not exist it falls back to the same path relative to the running binary's repo
// (best-effort — ApplyPlan/Health re-check existence).
func resolveActuatorScript(workspaceRoot string) string {
	const rel = "scripts/lms-actuator/lms-actuator.mjs"
	if workspaceRoot != "" {
		cand := filepath.Join(workspaceRoot, rel)
		if statOK(cand) {
			return cand
		}
	}
	// Fall back to a git-root probe from cwd.
	if root, err := gitTopLevel(); err == nil {
		cand := filepath.Join(root, rel)
		if statOK(cand) {
			return cand
		}
		return cand // return the computed path even if absent; Health reports it
	}
	if workspaceRoot != "" {
		return filepath.Join(workspaceRoot, rel)
	}
	return ""
}

// gitTopLevel returns the git repository root of the current working directory.
func gitTopLevel() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
