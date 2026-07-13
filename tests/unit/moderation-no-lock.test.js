// Guards: content-moderation rejections from a gateway (e.g. AgentRouter on Claude models)
// must NOT lock the account. They are deterministic — retrying the same prompt is pointless,
// and a 500 "sensitive words detected" used to fall through to the default 30s transient lock,
// freezing every subsequent request to that connection. See open-sse/config/errorConfig.js.
import { describe, it, expect } from "vitest";
import { checkFallbackError } from "../../open-sse/services/accountFallback.js";

describe("content-moderation errors do not lock the account", () => {
  // Exact message shapes parseUpstreamError surfaces (json.error.message), incl. the request id tail.
  const cases = [
    [500, "sensitive words detected (request id: 20260711225007604732511mp8xbNfQDQPTM)"],
    [400, "content-blocked (request id: 20260713095458343779367jddvjN5zMRm1u)"],
    [500, "content blocked by upstream policy"],
    [200, "SENSITIVE WORD flagged"], // case-insensitive, status-agnostic
  ];

  for (const [status, msg] of cases) {
    it(`status ${status}: "${msg.slice(0, 30)}..." → no fallback, no cooldown`, () => {
      const r = checkFallbackError(status, msg, 0);
      expect(r.shouldFallback).toBe(false);
      expect(r.cooldownMs).toBe(0);
    });
  }

  it("regression: a generic 500 still locks (transient), proving the rules are specific", () => {
    const r = checkFallbackError(500, "Internal server error", 0);
    expect(r.shouldFallback).toBe(true);
    expect(r.cooldownMs).toBeGreaterThan(0);
  });
});
