import fs from "node:fs";
import path from "node:path";
import initSqlJs from "sql.js";
import { PRAGMA_SQL } from "../schema.js";

let SQL = null;

async function loadSql() {
  if (SQL) return SQL;
  SQL = await initSqlJs();
  return SQL;
}

export async function createSqlJsAdapter(filePath) {
  const SQLLib = await loadSql();
  const buf = fs.existsSync(filePath) ? fs.readFileSync(filePath) : null;
  const db = new SQLLib.Database(buf);
  db.exec(PRAGMA_SQL);
  // Schema is created/synced by migrate.js after adapter init

  let dirty = false;
  let saveTimer = null;
  const SAVE_DEBOUNCE_MS = 100;
  // Persisting writes the entire serialized DB (sql.js has no incremental/WAL). To avoid a
  // truncated/corrupt data.sqlite on crash/power-loss mid-write (which would make the DB
  // unopenable and lose everything), write to a temp file in the SAME directory then atomically
  // rename over the live file. rename is atomic on POSIX/Windows when source+dest share a
  // filesystem, so a reader always sees either the old or the new complete file — never a tear.
  const tmpPath = `${filePath}.${process.pid}.tmp`;

  function persist() {
    const data = db.export();
    fs.writeFileSync(tmpPath, Buffer.from(data));
    fs.renameSync(tmpPath, filePath);
    dirty = false;
  }

  function scheduleSave() {
    dirty = true;
    if (saveTimer) clearTimeout(saveTimer);
    saveTimer = setTimeout(() => {
      saveTimer = null;
      if (dirty) {
        try { persist(); } catch (e) { console.error("[sqljs] save failed:", e); }
      }
    }, SAVE_DEBOUNCE_MS);
  }

  function paramsObj(params) {
    if (!params || (Array.isArray(params) && params.length === 0)) return undefined;
    return params;
  }

  function run(sql, params = []) {
    const stmt = db.prepare(sql);
    try {
      stmt.bind(paramsObj(params));
      stmt.step();
      const changes = db.getRowsModified();
      const lastInsertRowid = db.exec("SELECT last_insert_rowid() as id")[0]?.values?.[0]?.[0] ?? null;
      scheduleSave();
      return { changes, lastInsertRowid };
    } finally {
      stmt.free();
    }
  }

  function get(sql, params = []) {
    const stmt = db.prepare(sql);
    try {
      stmt.bind(paramsObj(params));
      if (stmt.step()) return stmt.getAsObject();
      return undefined;
    } finally {
      stmt.free();
    }
  }

  function all(sql, params = []) {
    const stmt = db.prepare(sql);
    try {
      stmt.bind(paramsObj(params));
      const rows = [];
      while (stmt.step()) rows.push(stmt.getAsObject());
      return rows;
    } finally {
      stmt.free();
    }
  }

  function exec(sql) {
    db.exec(sql);
    scheduleSave();
  }

  function transaction(fn) {
    const sp = `sp_${Math.random().toString(36).slice(2)}`;
    db.exec(`SAVEPOINT ${sp}`);
    try {
      const result = fn();
      db.exec(`RELEASE ${sp}`);
      scheduleSave();
      return result;
    } catch (e) {
      try { db.exec(`ROLLBACK TO ${sp}`); db.exec(`RELEASE ${sp}`); } catch {}
      throw e;
    }
  }

  function close() {
    if (saveTimer) clearTimeout(saveTimer);
    saveTimer = null;
    if (dirty) {
      try { persist(); } catch (e) { console.error("[sqljs] flush on close failed:", e); }
    }
    db.close();
  }

  // Flush on shutdown. Without this, the 100ms debounce window (and any write since the last
  // persist) is lost when the process exits via process.exit(), an uncaught exception, or a
  // hard container stop that never delivers beforeExit. Use `once` + process.exit(0) (matching
  // the better-sqlite3/bun/node adapters) so a second signal during the synchronous persist()
  // cannot re-enter flush and run a second db.export() concurrently.
  let flushing = false;
  const flush = (signal) => {
    if (flushing) return; // re-entrancy guard for repeated signals
    flushing = true;
    if (dirty) {
      try { persist(); } catch (e) { console.error("[sqljs] shutdown flush failed:", e); }
    }
    // For termination signals, exit cleanly so no handler runs twice and the process doesn't
    // linger after the DB is safely persisted. beforeExit must NOT call exit (it's a lifecycle
    // hook, not a termination signal).
    if (signal && signal !== "beforeExit") process.exit(0);
  };
  process.on("beforeExit", () => flush("beforeExit"));
  process.once("SIGINT", () => flush("SIGINT"));
  process.once("SIGTERM", () => flush("SIGTERM"));

  return { driver: "sql.js", run, get, all, exec, transaction, close, raw: db };
}
