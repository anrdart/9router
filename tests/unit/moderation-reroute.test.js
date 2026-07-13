// Guards content-moderation reroute config: a provider that content-blocks (AgentRouter) must
// remap to a non-moderated twin serving the same model, keeping the client model name identical.
// The handler branch (src/sse/handlers/chat.js) is integration-tested manually; this locks the
// pure config so the mapping can't silently drift.
import { describe, it, expect } from "vitest";
import { isModerationError, MODERATION_FALLBACK, MODERATION_SIGNATURES } from "../../open-sse/config/errorConfig.js";

describe("isModerationError", () => {
  it("matches the exact upstream shapes (as wrapped by formatProviderError)", () => {
    expect(isModerationError("[400]: content-blocked (request id: 20260713...)")).toBe(true);
    expect(isModerationError("sensitive words detected (request id: ...)")).toBe(true);
    expect(isModerationError('{"error":{"code":"content_blocked"}}')).toBe(true);
  });
  it("does not match unrelated errors", () => {
    expect(isModerationError("[429]: rate limited")).toBe(false);
    expect(isModerationError("[500]: Internal server error")).toBe(false);
    expect(isModerationError(null)).toBe(false);
    expect(isModerationError("")).toBe(false);
  });
  it("every signature is itself detected (no dead entries)", () => {
    for (const sig of MODERATION_SIGNATURES) expect(isModerationError(sig)).toBe(true);
  });
});

describe("MODERATION_FALLBACK.agentrouter", () => {
  const f = MODERATION_FALLBACK.agentrouter;
  it("remaps claude models to the claude provider, same model", () => {
    expect(f("claude-opus-4-8")).toBe("claude/claude-opus-4-8");
    expect(f("claude-opus-4-6")).toBe("claude/claude-opus-4-6");
  });
  it("remaps glm models to the glm provider, same model", () => {
    expect(f("glm-5.2")).toBe("glm/glm-5.2");
  });
  it("returns null when no clean twin exists (don't silently mangle)", () => {
    expect(f("gpt-5.5")).toBeNull();
    expect(f("")).toBeNull();
    expect(f(null)).toBeNull();
  });
});
