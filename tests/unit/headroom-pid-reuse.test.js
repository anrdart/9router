import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

// Regression guard for pid reuse: a stale proxy.pid pointing at a live process
// that is NOT headroom (the OS recycled the pid — observed: a VS Code renderer
// reusing pid 3600) must be treated as "not running" and cleared, so a fresh
// start spawns a real proxy instead of falsely reporting alreadyRunning.

const mocks = vi.hoisted(() => ({
  existsSync: vi.fn(),
  readFileSync: vi.fn(),
  writeFileSync: vi.fn(),
  unlinkSync: vi.fn(),
  mkdirSync: vi.fn(),
}));

vi.mock("fs", () => ({
  default: {
    existsSync: mocks.existsSync,
    readFileSync: mocks.readFileSync,
    writeFileSync: mocks.writeFileSync,
    unlinkSync: mocks.unlinkSync,
    mkdirSync: mocks.mkdirSync,
  },
  existsSync: mocks.existsSync,
  readFileSync: mocks.readFileSync,
  writeFileSync: mocks.writeFileSync,
  unlinkSync: mocks.unlinkSync,
  mkdirSync: mocks.mkdirSync,
}));

vi.mock("@/lib/dataDir.js", () => ({ DATA_DIR: "/tmp/9r-test" }));
vi.mock("@/lib/headroom/detect.js", () => ({ findHeadroomBinary: () => "/usr/bin/headroom" }));

import { getManagedPid } from "../../src/lib/headroom/process.js";

const PID = 3600;

beforeEach(() => {
  vi.restoreAllMocks();
  mocks.existsSync.mockReturnValue(true);
  mocks.unlinkSync.mockReset();
  // pidfile holds PID
  mocks.readFileSync.mockImplementation((p) => {
    if (String(p).endsWith("proxy.pid")) return String(PID);
    if (String(p).includes("/proc/")) return CMDLINE; // set per test
    throw new Error("unexpected read " + p);
  });
  // pid is alive
  vi.spyOn(process, "kill").mockImplementation(() => true);
});

afterEach(() => vi.clearAllMocks());

let CMDLINE = "";

describe("getManagedPid — pid reuse", () => {
  it("returns null and clears pidfile when the live pid is NOT headroom", () => {
    // Only meaningful on linux (reads /proc/<pid>/cmdline). Skip elsewhere.
    if (process.platform !== "linux") return;
    CMDLINE = "/usr/share/code/code --type=renderer\x00--crashpad-handler";
    const pid = getManagedPid();
    expect(pid).toBeNull();
    expect(mocks.unlinkSync).toHaveBeenCalled(); // stale pidfile cleared
  });

  it("returns the pid when the live process IS headroom", () => {
    if (process.platform !== "linux") return;
    CMDLINE = "/home/u/.local/bin/headroom\x00--port\x008787";
    const pid = getManagedPid();
    expect(pid).toBe(PID);
    expect(mocks.unlinkSync).not.toHaveBeenCalled();
  });

  it("returns null when the pid is dead (kill throws)", () => {
    process.kill.mockImplementation(() => { throw new Error("ESRCH"); });
    expect(getManagedPid()).toBeNull();
  });
});
