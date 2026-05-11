# vLLM as a CogOS Inference Provider

vLLM is a first-class provider type in the CogOS kernel. The `"vllm"`
type label routes through the shared OpenAI-compatible dispatch path
(`internal/engine/provider_openai.go`); no dedicated provider
implementation is required for the unsupervised case (the operator runs
`vllm serve` themselves).

This page covers the unsupervised setup. A future `"vllm-supervised"`
provider type will add launchd / systemd lifecycle management, mirroring
how `mlx-supervised` works today.

## When to use vLLM

- **Linux / CUDA nodes**: vLLM is the recommended local-tier provider.
  PagedAttention, native prefix caching, and chunked prefill give better
  throughput than Ollama under continuous-running autonomic workloads.
- **LAN-attached inference box**: a remote vLLM endpoint is just an
  OpenAI-compatible HTTP target. Set `endpoint` to the box's address.
- **Apple Silicon laptop**: vLLM's Metal backend is not production-ready
  for continuous-running autonomic work. Prefer `mlx-supervised`
  locally and route to a remote vLLM endpoint for heavier inference.

## Prerequisites

Install vLLM and confirm it can serve a model. The minimum setup is:

```bash
pip install vllm
vllm serve google/gemma-2-9b-it \
    --host 127.0.0.1 \
    --port 8000 \
    --enable-prefix-caching
```

vLLM speaks the OpenAI `/v1/chat/completions` and `/v1/models` API on
the configured port. The kernel reaches it through the standard
`OpenAICompatProvider`.

Recommended startup flags for substrate use:

| Flag | Purpose |
|------|---------|
| `--host 127.0.0.1` | Loopback-only listener for laptop / single-node nodes. |
| `--port 8000` | Default port; align with `providers.local.yaml`. |
| `--enable-prefix-caching` | Reuse KV cache across dispatches with shared identity / role prefixes. The substrate is designed to benefit from this — see Phase 2 of the migration plan. |
| `--enable-chunked-prefill` | Smooths long-prompt latency under concurrent load. |
| `--max-model-len <N>` | Cap context window if you need to bound VRAM. |

## Configuration

Add a `vllm` block to `.cog/config/providers.local.yaml`. Example:

```yaml
providers:
  vllm-local:
    type: vllm
    enabled: true
    endpoint: "http://localhost:8000"
    model: "google/gemma-2-9b-it"
    timeout: 300
    context_window: 32768

routing:
  prefer_local: true
  fallback_chain:
    - vllm-local
    - ollama
    - anthropic
```

Key fields:

- **`type: vllm`** — registers under the OpenAI-compat dispatch path.
  The kernel uses this label in logs and metrics so vLLM is
  distinguishable from LM Studio / llama.cpp servers.
- **`endpoint`** — the vLLM server's HTTP base URL. Strip the trailing
  `/v1` if present; the kernel normalizes it.
- **`model`** — the model identifier vLLM advertises in `/v1/models`.
  Must exactly match the `--model` argument vLLM was started with.
- **`timeout`** — request timeout in seconds; bump to 300+ if you serve
  large prompts on a busy box.
- **`context_window`** — informational ceiling used by context
  assembly. Must be ≤ vLLM's `--max-model-len`.

## Remote vLLM (LAN)

Same shape, just point `endpoint` at the remote box:

```yaml
providers:
  vllm-remote:
    type: vllm
    enabled: true
    endpoint: "http://10.0.0.42:8000"
    model: "google/gemma-2-9b-it"
    timeout: 300
```

The kernel makes no distinction between a local and remote OpenAI-
compatible endpoint at dispatch time. Authentication is via the standard
`api_key_env` field if the remote endpoint requires a bearer token.

## Verifying

```bash
# Server health
curl -sf http://localhost:8000/v1/models | jq .

# Roundtrip a request through the kernel router
./scripts/cog infer --model vllm-local "hello"
```

If the model is listed at `/v1/models` and `infer` completes, vLLM is
correctly registered.

## Caveats

- **GPU / VRAM**: vLLM allocates per-model VRAM up front. Coexisting
  multiple model servers on one GPU requires manual fraction allocation
  (`--gpu-memory-utilization 0.5`).
- **No kernel lifecycle**: the unsupervised type does not start or
  restart vLLM. If the server crashes, the router marks it unavailable;
  fallback chains continue dispatching to whatever remains.
- **Default model fallback**: removing Ollama from the fallback chain is
  a Phase 4 migration step. Until then, leave Ollama in the fallback so
  the kernel stays responsive when vLLM is loading or restarting.

## See also

- `docs/PROVIDER-SPEC.md` — full Provider interface spec.
- `docs/writing-a-provider.md` — guide for adding new provider types.
- `internal/engine/router.go` — `makeProvider` registration switch.
- `internal/engine/provider_openai.go` — the dispatch path vLLM uses.
- `internal/engine/provider_vllm_test.go` — dispatch parity tests.
