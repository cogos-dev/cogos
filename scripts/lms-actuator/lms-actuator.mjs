#!/usr/bin/env node
// lms-actuator.mjs — CogOS lms-model-state actuator.
//
// The mutating half of the lms-model-state reconciler. The Go engine provider
// (internal/engine/provider_lms_model_state.go) computes a plan from a read-only
// probe and, on a drift action, execs this script to load / unload / re-load a
// model at a target context length over the @lmstudio/sdk websocket bridge.
//
// Usage:
//   node lms-actuator.mjs <load|unload|set-context> \
//        --host <ip> --port <1234> --model <id> \
//        [--context-length N] [--parallel P] [--ttl SECONDS] [--dry-run]
//
// Auth: the LM Studio remote-access passkey/token is read from the environment
// variable LMS_ACTUATOR_TOKEN (never passed on argv). It is threaded to the SDK
// as clientPasskey, which is how LM Studio's remote-access gate authenticates a
// websocket client.
//
// Verbs:
//   load         — ensure <model> is loaded (at --context-length if given)
//   set-context  — LM Studio has no live context resize, so this is unload+load
//                  at the new context length
//   unload       — unload <model>
//   list         — READ-ONLY: print the currently loaded models as JSON and exit
//                  (used by the connection self-test; never mutates)
//
// Output: a single JSON line on stdout, e.g.
//   {"ok":true,"op":"load","model":"...","contextLength":262144}
// On failure: {"ok":false,"op":"...","error":"..."} and a non-zero exit code.
//
// GUARDRAIL: pass --dry-run to resolve the client + connect + print the plan
// WITHOUT issuing any load/unload. The Go test harness and the operator's
// live-verification step use --dry-run (and the `list` verb) so no real load is
// ever triggered against a live backend during automated runs.

import { LMStudioClient } from "@lmstudio/sdk";

function parseArgs(argv) {
  const args = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith("--")) {
      const key = a.slice(2);
      const next = argv[i + 1];
      if (next === undefined || next.startsWith("--")) {
        args[key] = true; // boolean flag
      } else {
        args[key] = next;
        i++;
      }
    } else {
      args._.push(a);
    }
  }
  return args;
}

function emit(obj, code = 0) {
  process.stdout.write(JSON.stringify(obj) + "\n");
  process.exit(code);
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const op = args._[0];

  if (!op || !["load", "unload", "set-context", "list"].includes(op)) {
    emit({ ok: false, error: `unknown or missing op: ${op ?? "<none>"}` }, 2);
  }

  const host = args.host || "127.0.0.1";
  const port = String(args.port || "1234");
  const model = args.model;
  const contextLength = args["context-length"]
    ? parseInt(args["context-length"], 10)
    : undefined;
  const ttl = args.ttl ? parseInt(args.ttl, 10) : undefined;
  const dryRun = Boolean(args["dry-run"]);
  const token = process.env.LMS_ACTUATOR_TOKEN || "";

  const baseUrl = `ws://${host}:${port}`;

  // clientPasskey carries the remote-access token; clientIdentifier is a stable
  // label so the operator can see which client is connected.
  const clientOpts = {
    baseUrl,
    clientIdentifier: "cogos-lms-actuator",
  };
  if (token) {
    clientOpts.clientPasskey = token;
  }

  let client;
  try {
    client = new LMStudioClient(clientOpts);
  } catch (err) {
    emit({ ok: false, op, error: `client init: ${err?.message ?? err}` }, 1);
  }

  // READ-ONLY verb: list loaded models. Also the connection self-test used by
  // the test harness / operator verification (no mutation).
  if (op === "list") {
    try {
      const loaded = await client.llm.listLoaded();
      const ids = loaded.map((m) => m.identifier ?? m.modelKey ?? String(m));
      emit({ ok: true, op: "list", baseUrl, loaded: ids });
    } catch (err) {
      emit({ ok: false, op: "list", baseUrl, error: err?.message ?? String(err) }, 1);
    }
  }

  if (!model) {
    emit({ ok: false, op, error: "missing --model" }, 2);
  }

  // GUARDRAIL: dry-run resolves everything and prints the plan but never issues
  // a load/unload.
  if (dryRun) {
    emit({
      ok: true,
      op,
      dryRun: true,
      baseUrl,
      model,
      contextLength: contextLength ?? null,
      ttl: ttl ?? null,
      note: "dry-run: no load/unload issued",
    });
  }

  try {
    switch (op) {
      case "unload": {
        await client.llm.unload(model);
        emit({ ok: true, op, model, baseUrl });
        break;
      }
      case "set-context": {
        // LM Studio has no live context resize: unload then reload at the new
        // context length. Unload is best-effort (model may not be loaded yet).
        try {
          await client.llm.unload(model);
        } catch (_) {
          /* not loaded — proceed to load */
        }
        const cfg = {};
        if (contextLength) cfg.contextLength = contextLength;
        const opts = { config: cfg };
        if (ttl) opts.ttl = ttl;
        await client.llm.load(model, opts);
        emit({ ok: true, op, model, contextLength: contextLength ?? null, baseUrl });
        break;
      }
      case "load": {
        const cfg = {};
        if (contextLength) cfg.contextLength = contextLength;
        const opts = { config: cfg };
        if (ttl) opts.ttl = ttl;
        await client.llm.load(model, opts);
        emit({ ok: true, op, model, contextLength: contextLength ?? null, baseUrl });
        break;
      }
      default:
        emit({ ok: false, op, error: "unreachable" }, 2);
    }
  } catch (err) {
    emit({ ok: false, op, model, baseUrl, error: err?.message ?? String(err) }, 1);
  }
}

main().catch((err) => {
  emit({ ok: false, error: `unhandled: ${err?.message ?? err}` }, 1);
});
