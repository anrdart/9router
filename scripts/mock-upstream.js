#!/usr/bin/env node
// scripts/mock-upstream.js
// A tiny mock of the Node (open-sse) engine used ONLY for verifying the Go
// reverse-proxy bridge end-to-end without needing the full Next.js app.
// It serves a few canned endpoints on the port given as argv[2] (default 20129).

const http = require("node:http");

const PORT = parseInt(process.argv[2] || "20129", 10);

const server = http.createServer((req, res) => {
  // /v1/models → a minimal OpenAI-style list
  if (req.url === "/v1/models" && req.method === "GET") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({
      object: "list",
      data: [{ id: "mock-model", object: "model", owned_by: "mock" }],
    }));
    return;
  }

  // /v1/chat/completions streaming → SSE chunks + [DONE]
  if (req.url === "/v1/chat/completions" && req.method === "POST") {
    res.writeHead(200, {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache",
      "Connection": "keep-alive",
    });
    const chunks = ["Hello", " world", "!"];
    let i = 0;
    const timer = setInterval(() => {
      if (i < chunks.length) {
        res.write(`data: ${JSON.stringify({ choices: [{ delta: { content: chunks[i++] } }] })}\n\n`);
      } else {
        res.write("data: [DONE]\n\n");
        clearInterval(timer);
        res.end();
      }
    }, 50);
    // If the client disconnects, stop writing.
    req.on("close", () => clearInterval(timer));
    return;
  }

  // echo headers for redaction verification (intentionally includes auth for the test)
  if (req.url === "/debug/headers") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(req.headers));
    return;
  }

  res.writeHead(404, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ error: "not found", path: req.url }));
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`mock-upstream listening on http://127.0.0.1:${PORT}`);
});
