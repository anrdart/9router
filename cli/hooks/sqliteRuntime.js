// Ensure better-sqlite3 is installed in USER_DATA_DIR/runtime/node_modules
// (user-writable, avoids Windows EBUSY locks during `bun i -g` updates).
// sql.js is bundled in bin/app already; node:sqlite / bun:sqlite are built-in.
const { execSync, spawnSync } = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");

const BETTER_SQLITE3_VERSION = "12.6.2";
const SQL_JS_VERSION = "1.14.1";

function getDataDir() {
  if (process.env.DATA_DIR) return process.env.DATA_DIR;
  return process.platform === "win32"
    ? path.join(process.env.APPDATA || os.homedir(), "9router")
    : path.join(os.homedir(), ".9router");
}

function getRuntimeDir() {
  return path.join(getDataDir(), "runtime");
}

function getRuntimeNodeModules() {
  return path.join(getRuntimeDir(), "node_modules");
}

function ensureRuntimeDir() {
  const dir = getRuntimeDir();
  if (!fs.existsSync(dir)) fs.mkdirSync(dir, { recursive: true });

  // Minimal package.json so the package manager treats it as a project root
  const pkgPath = path.join(dir, "package.json");
  if (!fs.existsSync(pkgPath)) {
    fs.writeFileSync(pkgPath, JSON.stringify({
      name: "9router-runtime",
      version: "1.0.0",
      private: true,
      description: "User-writable runtime deps for 9router (better-sqlite3 native binary)",
    }, null, 2));
  }
  return dir;
}

function hasModule(name) {
  return fs.existsSync(path.join(getRuntimeNodeModules(), name, "package.json"));
}

function isBetterSqliteBinaryValid() {
  const binary = path.join(getRuntimeNodeModules(), "better-sqlite3", "build", "Release", "better_sqlite3.node");
  if (!fs.existsSync(binary)) return false;
  try {
    const fd = fs.openSync(binary, "r");
    const buf = Buffer.alloc(4);
    fs.readSync(fd, buf, 0, 4, 0);
    fs.closeSync(fd);
    const magic = buf.toString("hex");
    if (process.platform === "linux") return magic.startsWith("7f454c46");
    if (process.platform === "darwin") return magic.startsWith("cffaedfe") || magic.startsWith("cefaedfe");
    if (process.platform === "win32") return magic.startsWith("4d5a");
    return true;
  } catch { return false; }
}

// Extract a short, user-friendly reason from bun stderr.
function summarizeNpmError(stderr = "") {
  const text = String(stderr);
  if (/ENOTFOUND|ETIMEDOUT|EAI_AGAIN|network|getaddrinfo|failed to (resolve|download)/i.test(text)) return "No internet connection or registry unreachable";
  if (/EACCES|EPERM|permission denied/i.test(text)) return "Permission denied (check folder permissions)";
  if (/ENOSPC|no space/i.test(text)) return "Not enough disk space";
  if (/node-gyp|gyp ERR|python|MSBuild|Visual Studio|Xcode|node-api|prebuild/i.test(text)) return "Missing build tools (Xcode CLT / Python / VS Build Tools)";
  if (/ETARGET|version.*not found|No version matching/i.test(text)) return "Package version not found on registry";
  const m = text.match(/(?:npm ERR!|error:) (.+)/i);
  if (m) return m[1].slice(0, 200);
  const lastLine = text.trim().split(/\r?\n/).filter(Boolean).pop();
  return lastLine ? lastLine.slice(0, 200) : "Unknown error";
}

// Install packages into the user-writable runtime dir with Bun. `bun add` saves
// to the runtime package.json and builds trusted native modules (better-sqlite3,
// systray2 are on Bun's default-trusted list); `--no-save` is used for optional
// installs we don't want persisted. Bun has no --no-audit/--no-fund/--prefer-online
// equivalents (it never audits and always checks the registry), so they are dropped.
function runNpmInstall({ cwd, pkgs, extraArgs = [], timeout = 180000 }) {
  const args = ["add", ...pkgs, ...extraArgs];
  const bunCmd = process.platform === "win32" ? "bun.exe" : "bun";
  const res = spawnSync(bunCmd, args, {
    cwd,
    stdio: ["ignore", "pipe", "pipe"],
    timeout,
    shell: process.platform === "win32",
    encoding: "utf8",
  });
  return { ok: res.status === 0, code: res.status, stderr: res.stderr || "", stdout: res.stdout || "" };
}

function npmInstall(pkgs, opts = {}) {
  const cwd = ensureRuntimeDir();
  const extra = opts.optional ? ["--no-save"] : [];
  if (!opts.silent) console.log("⏳ Installing SQLite engine (first run)...");
  const res = runNpmInstall({ cwd, pkgs, extraArgs: extra, timeout: opts.timeout || 180000 });
  if (!res.ok && !opts.silent) {
    const reason = summarizeNpmError(res.stderr);
    console.warn("⚠️  SQLite engine install failed — using fallback");
    console.warn(`   Reason: ${reason}`);
    console.warn(`   Retry:  cd "${cwd}" && bun add ${pkgs.join(" ")}`);
  }
  return res.ok;
}

// Public: ensure better-sqlite3 native module is installed in user-writable
// runtime dir. sql.js may be bundled in bin/app, but publishing strips .wasm
// from nested node_modules — verify and reinstall if missing. node:sqlite is
// built-in. This is purely a *speed optimization* — app works without
// better-sqlite3 via fallbacks.
function isSqlJsWasmValid() {
  const bundledWasm = path.join(__dirname, "..", "app", "node_modules", "sql.js", "dist", "sql-wasm.wasm");
  if (fs.existsSync(bundledWasm)) return true;
  const runtimeWasm = path.join(getRuntimeNodeModules(), "sql.js", "dist", "sql-wasm.wasm");
  return fs.existsSync(runtimeWasm);
}

function ensureSqliteRuntime({ silent = false } = {}) {
  ensureRuntimeDir();

  let sqlJsOk = isSqlJsWasmValid();
  if (!sqlJsOk) {
    sqlJsOk = npmInstall([`sql.js@${SQL_JS_VERSION}`], { silent });
    if (sqlJsOk) sqlJsOk = isSqlJsWasmValid();
  }

  const needBetterSqlite = !hasModule("better-sqlite3") || !isBetterSqliteBinaryValid();
  if (!needBetterSqlite) {
    if (!silent) console.log("✅ SQLite engine ready");
    return { betterSqlite: true, sqlJs: sqlJsOk };
  }

  const ok = npmInstall([`better-sqlite3@${BETTER_SQLITE3_VERSION}`], { optional: true, silent });
  return {
    betterSqlite: ok && hasModule("better-sqlite3") && isBetterSqliteBinaryValid(),
    sqlJs: sqlJsOk,
  };
}

// Inject runtime + bundled node_modules into NODE_PATH so child Node processes
// resolve sql.js (bundled in bin/app/node_modules) and better-sqlite3 (runtime).
function buildEnvWithRuntime(baseEnv = process.env) {
  const runtimeNm = getRuntimeNodeModules();
  const bundledNm = path.join(__dirname, "..", "app", "node_modules");
  const existing = baseEnv.NODE_PATH || "";
  const NODE_PATH = [runtimeNm, bundledNm, existing].filter(Boolean).join(path.delimiter);
  return { ...baseEnv, NODE_PATH };
}

module.exports = {
  ensureSqliteRuntime,
  buildEnvWithRuntime,
  getRuntimeDir,
  getRuntimeNodeModules,
  runNpmInstall,
  summarizeNpmError,
};
