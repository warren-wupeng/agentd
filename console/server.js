// agentd console — zero-dependency Node.js server (M8).
// Serves the static frontend and reverse-proxies /api/* to the Go
// control plane (streaming, so SSE works through it). No build step:
// the boring philosophy applies to consoles too.
//
//   AGENTD_URL=http://localhost:8080 PORT=5177 node server.js

const http = require("http");
const fs = require("fs");
const path = require("path");

const AGENTD_URL = (process.env.AGENTD_URL || "http://localhost:8080").replace(/\/$/, "");
const PORT = parseInt(process.env.PORT || "5177", 10);
const PUBLIC = path.join(__dirname, "public");

const MIME = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json",
  ".svg": "image/svg+xml",
};

function proxy(req, res) {
  const target = new URL(req.url.replace(/^\/api/, ""), AGENTD_URL);
  const upstream = http.request(
    target,
    { method: req.method, headers: { ...req.headers, host: target.host } },
    (up) => {
      res.writeHead(up.statusCode, up.headers);
      up.pipe(res); // streams — SSE frames flow through
    }
  );
  upstream.on("error", (err) => {
    res.writeHead(502, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: { code: "BAD_GATEWAY", message: String(err), remediation: "is the Go API running at " + AGENTD_URL + "?" } }));
  });
  req.pipe(upstream);
}

function serveStatic(req, res) {
  let p = req.url === "/" ? "/index.html" : req.url;
  const file = path.normalize(path.join(PUBLIC, p));
  if (!file.startsWith(PUBLIC)) {
    res.writeHead(403).end();
    return;
  }
  fs.readFile(file, (err, data) => {
    if (err) {
      res.writeHead(404, { "Content-Type": "text/plain" });
      res.end("not found");
      return;
    }
    res.writeHead(200, { "Content-Type": MIME[path.extname(file)] || "application/octet-stream" });
    res.end(data);
  });
}

http
  .createServer((req, res) => {
    if (req.url.startsWith("/api/")) proxy(req, res);
    else serveStatic(req, res);
  })
  .listen(PORT, () => console.log(`agentd console on http://localhost:${PORT} → ${AGENTD_URL}`));
