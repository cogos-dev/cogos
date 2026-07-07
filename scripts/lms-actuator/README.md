# lms-actuator

The mutating half of the CogOS `lms-model-state` reconciler (ADR-103). It
load/unload/re-loads LM Studio models at a target context length over the
`@lmstudio/sdk` websocket bridge, on behalf of
`internal/engine/provider_lms_model_state.go`'s `ApplyPlan`.

## Setup

```bash
npm install
```

## Usage

```bash
node lms-actuator.mjs <load|unload|set-context|list> \
     --host <ip> --port 1234 --model <id> \
     [--context-length N] [--parallel P] [--ttl SECONDS] [--dry-run]
```

- Auth token is read from the `LMS_ACTUATOR_TOKEN` environment variable and
  threaded to the SDK as `clientPasskey` — **never** passed on argv.
- `list` is read-only (lists loaded models) and `--dry-run` resolves + prints the
  plan without issuing any load/unload. Both are safe against a live backend and
  are used for connection self-tests.
- Output is a single JSON line on stdout.

## Testing without a live backend

`mock-lms-server.mjs` is a minimal websocket mock that completes the SDK auth
handshake and answers a read-only `listLoaded` — enough to prove the actuator can
connect and dispatch a read-only op without touching a real LM Studio instance:

```bash
MOCK_PORT=14577 node mock-lms-server.mjs &
LMS_ACTUATOR_TOKEN=test node lms-actuator.mjs list --host 127.0.0.1 --port 14577
# → {"ok":true,"op":"list","loaded":[]}
```
