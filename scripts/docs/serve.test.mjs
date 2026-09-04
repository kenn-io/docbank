import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import http from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { createRebuildQueue, startDocsServer } from "./serve.mjs";


async function write(root, relative, contents) {
  const destination = path.join(root, relative);
  await mkdir(path.dirname(destination), { recursive: true });
  await writeFile(destination, contents);
}


async function fixture(t) {
  const repoRoot = await mkdtemp(path.join(tmpdir(), "docbank-docs-server-"));
  t.after(() => rm(repoRoot, { recursive: true, force: true }));
  for (const [relative, contents] of Object.entries({
    "site/index.html": "<h1>Product</h1>",
    "site/guide/index.html": "<h1>Guide</h1>",
    "site/guide.md": "# Guide\n",
    "site/docs/index.html": "<h1>Docs</h1>",
    "site/docs/setup/index.html": "<h1>Setup</h1>",
    "site/docs/setup.md": "# Setup\n",
    "site/styles/site.css": "body { color: white; }\n",
    "site/fonts/Inter-Regular.woff2": "font fixture",
    "site/assets/generated/capture.png": "png fixture",
  })) {
    await write(repoRoot, relative, contents);
  }
  return repoRoot;
}


function rawRequest(port, requestPath) {
  return new Promise((resolve, reject) => {
    const request = http.get({ host: "127.0.0.1", port, path: requestPath }, (response) => {
      response.resume();
      response.once("end", () => resolve(response.statusCode));
    });
    request.once("error", reject);
  });
}


test("serves every public tier and asset type from one origin", async (t) => {
  const repoRoot = await fixture(t);
  const server = await startDocsServer({
    repoRoot,
    host: "127.0.0.1",
    port: 0,
    build: async () => {},
    watch: false,
  });
  t.after(() => server.close());

  for (const [route, contentType, body] of [
    ["/", "text/html; charset=utf-8", "Product"],
    ["/guide/", "text/html; charset=utf-8", "Guide"],
    ["/guide.md", "text/markdown; charset=utf-8", "# Guide"],
    ["/docs/", "text/html; charset=utf-8", "Docs"],
    ["/docs/setup/", "text/html; charset=utf-8", "Setup"],
    ["/docs/setup.md", "text/markdown; charset=utf-8", "# Setup"],
    ["/styles/site.css", "text/css; charset=utf-8", "body"],
    ["/fonts/Inter-Regular.woff2", "font/woff2", "font fixture"],
    ["/assets/generated/capture.png", "image/png", "png fixture"],
  ]) {
    const response = await fetch(new URL(route, server.url));
    assert.equal(response.status, 200, route);
    assert.equal(response.headers.get("content-type"), contentType, route);
    assert.match(await response.text(), new RegExp(body), route);
  }
});


test("rejects traversal outside the generated site", async (t) => {
  const repoRoot = await fixture(t);
  await write(repoRoot, "AGENTS.md", "private instructions\n");
  const server = await startDocsServer({
    repoRoot,
    host: "127.0.0.1",
    port: 0,
    build: async () => {},
    watch: false,
  });
  t.after(() => server.close());

  assert.equal(await rawRequest(server.port, "/../AGENTS.md"), 404);
  assert.equal(await rawRequest(server.port, "/%2e%2e/AGENTS.md"), 404);
  assert.equal(await rawRequest(server.port, "/%00"), 404);
});


test("coalesces file events into one queued rebuild", async () => {
  let calls = 0;
  let finishFirst;
  const firstBuild = new Promise((resolve) => {
    finishFirst = resolve;
  });
  const errors = [];
  const queue = createRebuildQueue({
    debounceMs: 5,
    build: async () => {
      calls += 1;
      if (calls === 1) await firstBuild;
    },
    onError: (error) => errors.push(error),
  });

  queue.request();
  await queue.idle({ waitForRunning: false });
  assert.equal(calls, 1);

  queue.request();
  queue.request();
  queue.request();
  finishFirst();
  await queue.idle();

  assert.equal(calls, 2);
  assert.deepEqual(errors, []);
  queue.close();
});


test("keeps the queue usable after a failed rebuild", async () => {
  let calls = 0;
  const errors = [];
  const queue = createRebuildQueue({
    debounceMs: 1,
    build: async () => {
      calls += 1;
      if (calls === 1) throw new Error("fixture build failed");
    },
    onError: (error) => errors.push(error.message),
  });

  queue.request();
  await queue.idle();
  queue.request();
  await queue.idle();

  assert.equal(calls, 2);
  assert.deepEqual(errors, ["fixture build failed"]);
  queue.close();
});
