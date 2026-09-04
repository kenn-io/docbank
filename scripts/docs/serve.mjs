import { createReadStream, watch as watchPath } from "node:fs";
import { lstat } from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import { buildSite } from "./build.mjs";


const contentTypes = new Map([
  [".css", "text/css; charset=utf-8"],
  [".html", "text/html; charset=utf-8"],
  [".js", "text/javascript; charset=utf-8"],
  [".json", "application/json; charset=utf-8"],
  [".md", "text/markdown; charset=utf-8"],
  [".png", "image/png"],
  [".ps1", "text/plain; charset=utf-8"],
  [".sh", "text/x-shellscript; charset=utf-8"],
  [".svg", "image/svg+xml"],
  [".txt", "text/plain; charset=utf-8"],
  [".woff2", "font/woff2"],
  [".xml", "application/xml; charset=utf-8"],
]);


export function createRebuildQueue({ build, debounceMs = 100, onError = console.error }) {
  let timer;
  let running = false;
  let pending = false;
  let closed = false;
  const waiters = new Set();

  function notify() {
    for (const waiter of waiters) waiter();
  }

  function schedule(delay = debounceMs) {
    if (timer) clearTimeout(timer);
    timer = setTimeout(run, delay);
  }

  async function run() {
    timer = undefined;
    if (closed) {
      notify();
      return;
    }
    running = true;
    notify();
    try {
      await build();
    } catch (error) {
      onError(error);
    } finally {
      running = false;
      if (pending && !closed) {
        pending = false;
        schedule(0);
      }
      notify();
    }
  }

  return {
    request() {
      if (closed) return;
      if (running) {
        pending = true;
        return;
      }
      schedule();
    },

    idle({ waitForRunning = true } = {}) {
      return new Promise((resolve) => {
        const check = () => {
          const settled = waitForRunning
            ? !timer && !running && !pending
            : running || (!timer && !pending);
          if (!settled) return;
          waiters.delete(check);
          resolve();
        };
        waiters.add(check);
        check();
      });
    },

    close() {
      closed = true;
      pending = false;
      if (timer) clearTimeout(timer);
      timer = undefined;
      notify();
    },
  };
}


function resolveRequest(siteRoot, rawUrl) {
  const rawPath = rawUrl.split(/[?#]/, 1)[0];
  let decoded;
  try {
    decoded = decodeURIComponent(rawPath);
  } catch {
    return undefined;
  }
  if (!decoded.startsWith("/") || decoded.includes("\0") || decoded.includes("\\")) {
    return undefined;
  }
  if (decoded.split("/").includes("..")) return undefined;

  const route = decoded.endsWith("/") ? `${decoded}index.html` : decoded;
  const target = path.resolve(siteRoot, `.${route}`);
  const prefix = `${path.resolve(siteRoot)}${path.sep}`;
  if (!target.startsWith(prefix)) return undefined;
  return target;
}


async function serveFile(siteRoot, request, response) {
  if (request.method !== "GET" && request.method !== "HEAD") {
    response.writeHead(405, { Allow: "GET, HEAD" });
    response.end();
    return;
  }

  const target = resolveRequest(siteRoot, request.url ?? "/");
  if (!target) {
    response.writeHead(404);
    response.end("Not found\n");
    return;
  }

  let metadata;
  try {
    metadata = await lstat(target);
  } catch (error) {
    if (error?.code === "ENOENT" || error?.code === "ENOTDIR") {
      response.writeHead(404);
      response.end("Not found\n");
      return;
    }
    throw error;
  }
  if (!metadata.isFile() || metadata.isSymbolicLink()) {
    response.writeHead(404);
    response.end("Not found\n");
    return;
  }

  response.writeHead(200, {
    "Cache-Control": "no-store",
    "Content-Length": metadata.size,
    "Content-Type": contentTypes.get(path.extname(target)) ?? "application/octet-stream",
  });
  if (request.method === "HEAD") {
    response.end();
    return;
  }
  createReadStream(target).pipe(response);
}


function listen(server, host, port) {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, host, () => {
      server.off("error", reject);
      resolve();
    });
  });
}


export async function startDocsServer({
  repoRoot,
  host = "127.0.0.1",
  port = 8000,
  build = ({ output }) => buildSite({ repoRoot, output }),
  watch = true,
  watchFactory = watchPath,
  logger = console,
}) {
  const siteRoot = path.join(repoRoot, "site");
  await build({ output: siteRoot });

  const queue = createRebuildQueue({
    build: async () => {
      try {
        await build({ output: siteRoot });
        logger.log("documentation rebuilt");
      } catch (error) {
        logger.error("documentation rebuild failed; serving the previous build");
        throw error;
      }
    },
    onError: (error) => logger.error(error),
  });

  const watchers = [];
  if (watch) {
    for (const [source, recursive] of [
      [path.join(repoRoot, "website"), true],
      [path.join(repoRoot, "docs"), true],
      [path.join(repoRoot, "scripts", "docs-assets.ref"), false],
      [path.join(repoRoot, "scripts", "docs-assets.txt"), false],
    ]) {
      const watcher = watchFactory(source, { recursive }, () => queue.request());
      watcher.on?.("error", (error) => logger.error(error));
      watchers.push(watcher);
    }
  }

  const server = http.createServer((request, response) => {
    serveFile(siteRoot, request, response).catch((error) => {
      logger.error(error);
      if (!response.headersSent) response.writeHead(500);
      response.end("Internal server error\n");
    });
  });
  await listen(server, host, port);
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("documentation server has no TCP address");
  const url = `http://${host}:${address.port}/`;

  return {
    url,
    port: address.port,
    close() {
      queue.close();
      for (const watcher of watchers) watcher.close();
      return new Promise((resolve, reject) => {
        server.close((error) => error ? reject(error) : resolve());
      });
    },
  };
}


const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const repoRoot = path.resolve(here, "..", "..");
  const configuredPort = process.env.DOCBANK_DOCS_PORT;
  const port = configuredPort === undefined ? 8000 : Number(configuredPort);
  if (!Number.isInteger(port) || port < 0 || port > 65_535) {
    throw new Error("DOCBANK_DOCS_PORT must be an integer from 0 through 65535");
  }
  const server = await startDocsServer({ repoRoot, port });
  process.stdout.write(`serving documentation at ${server.url}\n`);
}
