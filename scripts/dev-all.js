#!/usr/bin/env bun
// scripts/dev-all.js
// Runs the Go backend, the engine (open-sse upstream), and the Next.js dashboard
// together for local development — without any extra dependency.
//
//   go   :20128  (public entry point; native /health, proxies everything else)
//   node :20129  (open-sse engine, the reverse-proxy upstream)
//   ui   :20127  (Next.js dashboard dev server)
//
// Usage: bun run dev:all
// Stop with Ctrl-C; all three child processes are torn down together.

const { spawn } = require("node:child_process");
const path = require("node:path");

const ROOT = path.resolve(__dirname, "..");
const GO_BIN =
  process.env.GOROOT ? path.join(process.env.GOROOT, "bin", "go") : "go";

// The two Next.js dev servers run under Bun (`bun --bun next ...`) so the whole
// stack uses one runtime. process.execPath is the bun binary when launched via
// `bun run dev:all`.
const BUN_BIN = process.execPath;

const procs = [
  {
    name: "go",
    color: "\x1b[35m", // magenta
    cmd: GO_BIN,
    args: ["run", "./cmd/server"],
    opts: { cwd: path.join(ROOT, "backend"), env: { ...process.env } },
  },
  {
    name: "node",
    color: "\x1b[33m", // yellow
    cmd: BUN_BIN,
    args: ["--bun", "next", "dev", "--webpack", "--port", "20129"],
    opts: {
      cwd: ROOT,
      env: { ...process.env, PORT: "20129" },
    },
  },
  {
    name: "ui",
    color: "\x1b[36m", // cyan
    cmd: BUN_BIN,
    args: ["--bun", "next", "dev", "--webpack", "--port", "20127"],
    opts: { cwd: ROOT, env: { ...process.env } },
  },
];

const children = [];

function prefix(name, color) {
  return `${color}[${name}]\x1b[0m`;
}

for (const p of procs) {
  const child = spawn(p.cmd, p.args, {
    stdio: ["ignore", "pipe", "pipe"],
    ...p.opts,
    shell: process.platform === "win32",
  });
  children.push(child);

  const tag = prefix(p.name, p.color);
  child.stdout.on("data", (d) => process.stdout.write(d.toString().split("\n").map((l) => l.length ? `${tag} ${l}` : l).join("\n")));
  child.stderr.on("data", (d) => process.stderr.write(d.toString().split("\n").map((l) => l.length ? `${tag} ${l}` : l).join("\n")));
  child.on("exit", (code, signal) => {
    process.stderr.write(`${tag} exited (code=${code} signal=${signal})\n`);
  });
}

function killAll() {
  for (const c of children) {
    try {
      if (!c.killed) c.kill("SIGTERM");
    } catch (_) {
      /* ignore */
    }
  }
  process.exit(0);
}

process.on("SIGINT", killAll);
process.on("SIGTERM", killAll);
