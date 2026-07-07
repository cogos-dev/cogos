# LM Studio Model-State Reconciler (`lms-model-state`)

The `lms-model-state` reconciler keeps an LM Studio backend loaded with the
**model you declare, at the context length you declare** — the declarative
equivalent of hand-loading a model in the LM Studio desktop app or running the
imperative `com.cogos.lmstudio-baseline` launchd job.

It is **orthogonal to dispatch**. Dispatch (chat completions) still flows through
the existing `lmstudio` / `openai` provider on the same backend. This reconciler
adds a second concern on top: model/context state. See
[ADR-103](../adrs/103-lms-model-state-reconciler.md) for the design rationale.

> **This feature is OFF BY DEFAULT.** It activates only when a backend declares
> `options.model_state.manage: true`. Absent that, nothing is registered and the
> kernel reconciles no model state.

## How it works

| Method | What it does |
|--------|--------------|
| `FetchLive` | read-only `GET {endpoint}/api/v0/models` (LM Studio's native REST surface — exposes per-model `state` + `loaded_context_length`) |
| `ComputePlan` | diffs declared target vs live → `load` / `context` (unload+reload; LM Studio has no live resize) / `unload` (jit_evict) actions |
| `ApplyPlan` | shells the Node `@lmstudio/sdk` actuator (`scripts/lms-actuator/`) over `ws://`; token via `LMS_ACTUATOR_TOKEN` env, never argv |
| `Health` | O(1) from cached rows: Synced/Healthy when loaded at target; Degraded on wrong context; Missing when absent; Progressing while loading; **Suspended** when unmanaged, unreachable, or the actuator is not installed |

An **unreachable** backend reports **Suspended, not Degraded** — the autonomic
ticker does not try to self-heal a box that is simply off or off-LAN.

## Prerequisites

Install the actuator's SDK once:

```bash
cd scripts/lms-actuator
npm install
```

Verify the actuator can open a connection and do a read-only op **without loading
anything** (against a mock, or the `list` verb against a reachable backend):

```bash
# read-only: list loaded models (no mutation)
LMS_ACTUATOR_TOKEN=$ECLIPSE_API_KEY \
  node scripts/lms-actuator/lms-actuator.mjs list --host 192.168.10.191 --port 1234

# dry-run: resolve + print the plan, issue no load/unload
LMS_ACTUATOR_TOKEN=$ECLIPSE_API_KEY \
  node scripts/lms-actuator/lms-actuator.mjs load --host 192.168.10.191 --port 1234 \
       --model ornith-1.0-35b --context-length 262144 --dry-run
```

## Config (commented example — do NOT enable blindly)

Add this to `providers.local.yaml` and set `manage: true` **only** after the
actuator is installed and you have live-verified it against the target backend.

```yaml
providers:
  eclipse:
    type: openai                    # OpenAI-compatible dispatch (LM Studio REST)
    enabled: true
    endpoint: "http://192.168.10.191:1234"
    api_key_env: ECLIPSE_API_KEY    # Bearer token; also used by the reconciler
    model: "ornith-1.0-35b"
    timeout: 300
    context_window: 262144
    options:
      model_state:
        manage: true                # the opt-in switch; false/absent ⇒ Suspended
        model: "ornith-1.0-35b"
        context_length: 262144      # VERIFIED loaded + serving on the 24GB card
        parallel: 1
        keep_warm: true
        jit_evict: false            # true ⇒ unload a non-target model crowding the card
```

**Context length: use `262144`, not `65536`.** The old 65536 "ceiling" note was
refuted — Eclipse loads and serves `ornith-1.0-35b` at 262144 on the 24 GB card.
Darkstar is 262144 too.

### Local vs remote

- **Remote backend (Eclipse, any off-LAN):** always uses the Node SDK actuator.
  The `lms` CLI cannot reach a remote instance (LM Link gated).
- **Localhost backend (Darkstar):** may fast-path through
  `~/.lmstudio/bin/lms load … --context-length …` when the CLI is present.

## Two-writer hazard

Darkstar runs a `com.cogos.lmstudio-baseline` launchd job that loads a baseline
model at boot. If you enable this reconciler with a **different** target on the
same backend, the two writers race. Before trusting the reconciler, scope that
launchd job to boot-only (or retire it) so the reconciler is the single writer.
Do not enable both with divergent targets.

## Guardrails

1. The read path (`FetchLive`/`Health`/`ComputePlan`) is non-destructive. Only
   `ApplyPlan` mutates, and only via the external actuator.
2. Opt-in, off by default. No `model_state` block ⇒ Suspended, empty plan,
   nothing registered.
