#!/usr/bin/env node
// mock-lms-server.mjs — minimal LM Studio websocket mock for the actuator
// connection self-test. It exists ONLY to prove that lms-actuator.mjs can:
//   1. open a real ws:// connection (TCP + WS upgrade),
//   2. send the @lmstudio/sdk auth frame ({authVersion, clientIdentifier,
//      clientPasskey}) and be accepted,
//   3. dispatch a READ-ONLY rpcCall (listLoaded) — captured and logged here.
//
// It is NOT a full LM Studio implementation. It never loads or unloads a model
// (it has no models). This is the guardrail-compliant substitute for a live
// backend: the actuator's mutating verbs are never exercised against it.
//
// Prints one JSON line per observed protocol event to stderr; on the first
// listLoaded rpcCall it replies with an empty-array result and records success.

import { WebSocketServer } from "ws";

const port = parseInt(process.env.MOCK_PORT || "0", 10);
const wss = new WebSocketServer({ port });

const observed = { upgraded: false, authFrame: null, rpcCalls: [] };

wss.on("listening", () => {
  const addr = wss.address();
  process.stderr.write(JSON.stringify({ event: "listening", port: addr.port }) + "\n");
});

wss.on("connection", (ws) => {
  observed.upgraded = true;
  process.stderr.write(JSON.stringify({ event: "connection" }) + "\n");

  let authed = false;
  ws.on("message", (raw) => {
    let msg;
    try {
      msg = JSON.parse(raw.toString("utf-8"));
    } catch (e) {
      process.stderr.write(JSON.stringify({ event: "badjson", err: String(e) }) + "\n");
      return;
    }

    // First frame is the auth handshake.
    if (!authed && msg.authVersion !== undefined) {
      observed.authFrame = {
        authVersion: msg.authVersion,
        clientIdentifier: msg.clientIdentifier,
        hasPasskey: Boolean(msg.clientPasskey),
      };
      process.stderr.write(JSON.stringify({ event: "auth", frame: observed.authFrame }) + "\n");
      authed = true;
      ws.send(JSON.stringify({ success: true }));
      return;
    }

    // Subsequent frames are transport messages. We care about rpcCall.
    if (msg.type === "rpcCall") {
      observed.rpcCalls.push(msg.endpoint);
      process.stderr.write(
        JSON.stringify({ event: "rpcCall", endpoint: msg.endpoint, callId: msg.callId }) + "\n"
      );
      // Reply with an empty-array result for listLoaded. The SDK's per-endpoint
      // deserializer for listLoaded expects an array (Array<ModelInstanceInfo>);
      // an empty array is the valid "no models loaded" response.
      ws.send(
        JSON.stringify({
          type: "rpcResult",
          callId: msg.callId,
          result: [],
        })
      );
    } else if (msg.type === "keepAlive") {
      // ignore
    }
  });

  ws.on("close", () => {
    process.stderr.write(JSON.stringify({ event: "close", observed }) + "\n");
  });
});

// Auto-exit after a short window so the test never hangs.
setTimeout(() => {
  process.stderr.write(JSON.stringify({ event: "timeout-exit", observed }) + "\n");
  process.exit(0);
}, 4000);
