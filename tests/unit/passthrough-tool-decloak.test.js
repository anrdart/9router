/**
 * Regression test: streaming tool_use names must be DECLOAKED on the Claude→Claude
 * passthrough path.
 *
 * Bug: 9Router cloaks client tool names with an `_ide` suffix before sending to an
 * OAuth Claude connection (anti-ban). On the non-streaming path this is reversed
 * (nonStreamingHandler decloaks unconditionally), but the streaming passthrough
 * factory was never given the toolNameMap, so a same-format Claude stream emitted
 * `get_weather_ide` verbatim → clients like ZCode failed with "Tool not found".
 */

import { describe, it, expect } from "vitest";
import { createPassthroughStreamWithLogger } from "../../open-sse/utils/stream.js";
import { decloakParsedClaude } from "../../open-sse/utils/claudeCloaking.js";

// Drive a set of raw SSE lines through the passthrough TransformStream and collect
// the decoded output.
async function runStream(lines, toolNameMap) {
  const ts = createPassthroughStreamWithLogger(
    "claude", null, "claude-opus-4-8", null, {}, null, null, toolNameMap,
  );
  const enc = new TextEncoder();
  const dec = new TextDecoder();
  const writer = ts.writable.getWriter();
  const reader = ts.readable.getReader();

  let out = "";
  const pump = (async () => {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      out += dec.decode(value, { stream: true });
    }
  })();

  for (const line of lines) await writer.write(enc.encode(line));
  await writer.close();
  await pump;
  return out;
}

describe("passthrough stream tool decloak", () => {
  const toolUseEvent =
    'data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather_ide","input":{}}}\n\n';

  it("restores the original tool name from the toolNameMap", async () => {
    const map = new Map([["get_weather_ide", "get_weather"]]);
    const out = await runStream([toolUseEvent], map);
    expect(out).toContain('"name":"get_weather"');
    expect(out).not.toContain("get_weather_ide");
    // Still a valid content_block_start tool_use event.
    expect(out).toContain('"type":"tool_use"');
  });

  it("leaves tool names untouched when no map is provided", async () => {
    const out = await runStream([toolUseEvent], null);
    expect(out).toContain("get_weather_ide");
  });

  it("forwards non-tool events unchanged", async () => {
    const map = new Map([["get_weather_ide", "get_weather"]]);
    const text = 'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}\n\n';
    const done = "data: [DONE]\n\n";
    const out = await runStream([text, done], map);
    expect(out).toContain('"text":"hi"');
    expect(out).toContain("[DONE]");
  });

  it("decloaks a full JSON message body emitted at flush (client omitted stream)", async () => {
    // No trailing newline → the whole body sits in the buffer and is emitted by flush().
    const map = new Map([["get_weather_ide", "get_weather"]]);
    const fullMsg =
      '{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather_ide","input":{"city":"Tokyo"}}],"stop_reason":"tool_use"}';
    const out = await runStream([fullMsg], map);
    expect(out).toContain('"name":"get_weather"');
    expect(out).not.toContain("get_weather_ide");
  });
});

describe("decloakParsedClaude", () => {
  const map = new Map([["get_weather_ide", "get_weather"]]);

  it("restores name in a content_block_start event", () => {
    const ev = { type: "content_block_start", content_block: { type: "tool_use", name: "get_weather_ide" } };
    expect(decloakParsedClaude(ev, map)).toBe(true);
    expect(ev.content_block.name).toBe("get_weather");
  });

  it("restores names in a full message content array", () => {
    const msg = { type: "message", content: [{ type: "tool_use", name: "get_weather_ide" }, { type: "text", text: "x" }] };
    expect(decloakParsedClaude(msg, map)).toBe(true);
    expect(msg.content[0].name).toBe("get_weather");
  });

  it("returns false and mutates nothing when no name matches", () => {
    const msg = { type: "message", content: [{ type: "tool_use", name: "other" }] };
    expect(decloakParsedClaude(msg, map)).toBe(false);
    expect(msg.content[0].name).toBe("other");
  });

  it("is a no-op without a map", () => {
    expect(decloakParsedClaude({ type: "message", content: [] }, null)).toBe(false);
  });
});
