#!/usr/bin/env node
// Parity check: native Go /v1/models vs Node /v1/models against the same live DB.
//
// The Go backend serves /v1/models natively for the common case and reverse-
// proxies to Node when a connection needs live model resolution. This script
// hits both directly and diffs the sorted model-id sets so a divergence in the
// native aggregation is caught before shipping.
//
// Usage (with `npm run dev:all` running, or any two endpoints):
//   node scripts/verify-models-parity.mjs
//   GO_URL=http://127.0.0.1:20128 NODE_URL=http://127.0.0.1:20129 node scripts/verify-models-parity.mjs
//
// Exit 0 = identical id sets. Exit 1 = drift (prints the symmetric difference).
// Exit 2 = a request failed (endpoints not up).
//
// Note: when the Go response was itself proxied to Node (live-resolver case),
// the two are trivially identical — the check is still meaningful for the DBs
// where Go answers natively, which is what we care about.

const GO_URL = process.env.GO_URL || "http://127.0.0.1:20128";
const NODE_URL = process.env.NODE_URL || "http://127.0.0.1:20129";

async function fetchModels(base) {
  const url = `${base}/v1/models`;
  let res;
  try {
    res = await fetch(url, { headers: { accept: "application/json" } });
  } catch (e) {
    console.error(`✗ request to ${url} failed: ${e.message}`);
    process.exit(2);
  }
  if (!res.ok) {
    console.error(`✗ ${url} → HTTP ${res.status}`);
    process.exit(2);
  }
  const body = await res.json();
  const data = Array.isArray(body?.data) ? body.data : [];
  if (body?.object !== "list") {
    console.error(`✗ ${url}: expected {object:"list"}, got object=${body?.object}`);
    process.exit(1);
  }
  return data;
}

function idSet(models) {
  return new Set(models.map((m) => m?.id).filter((x) => typeof x === "string"));
}

function diff(a, b) {
  const only = [];
  for (const x of a) if (!b.has(x)) only.push(x);
  return only.sort();
}

const [goModels, nodeModels] = await Promise.all([
  fetchModels(GO_URL),
  fetchModels(NODE_URL),
]);

const go = idSet(goModels);
const node = idSet(nodeModels);

const onlyGo = diff(go, node);
const onlyNode = diff(node, go);

console.log(`Go   (${GO_URL}): ${go.size} model ids`);
console.log(`Node (${NODE_URL}): ${node.size} model ids`);

if (onlyGo.length === 0 && onlyNode.length === 0) {
  console.log("✓ id sets identical — parity confirmed");
  process.exit(0);
}

if (onlyNode.length) {
  console.error(`\n✗ ${onlyNode.length} id(s) in Node but NOT in Go (missing natively):`);
  for (const id of onlyNode) console.error("  - " + id);
}
if (onlyGo.length) {
  console.error(`\n✗ ${onlyGo.length} id(s) in Go but NOT in Node (extra):`);
  for (const id of onlyGo) console.error("  + " + id);
}
process.exit(1);
