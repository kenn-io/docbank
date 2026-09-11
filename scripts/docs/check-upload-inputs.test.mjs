import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { collectUploadInputs } from "./check-upload-inputs.mjs";
import { assertUploadBoundary } from "./assert-vercel-dry-run.mjs";

test("checks included local files and their sizes, including untracked inputs", async (t) => {
  const root = await mkdtemp(path.join(tmpdir(), "docbank-upload-inputs-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  await writeFile(path.join(root, ".vercelignore"), "/*\n!/website\n");
  await mkdir(path.join(root, "website"));
  await writeFile(path.join(root, "website", "index.html"), "hello");
  await writeFile(path.join(root, "excluded.txt"), "excluded");
  assert.deepEqual(await collectUploadInputs(root), [
    { path: "website/index.html", size: 5 },
  ]);

  await writeFile(path.join(root, ".vercelignore"), "/*\n!/website\n!/vercel.json\n");
  await writeFile(path.join(root, "vercel.json"), "{}");
  const required = [
    "docs/zensical.toml", "docs/uv.lock", "scripts/vercel-install-docs.sh",
    "scripts/vercel-build-docs.sh", "scripts/sync-docs-assets.sh",
    "scripts/docs-assets.ref", "scripts/docs-assets.txt", "scripts/docs/build.mjs",
    "scripts/docs/verify-site.mjs", "scripts/install.sh", "scripts/install.ps1",
  ].map((file) => ({ path: file, size: 1 }));
  const valid = [...required, ...await collectUploadInputs(root)];
  assert.doesNotThrow(() => assertUploadBoundary(valid));
  await writeFile(path.join(root, "website", "unexpected.txt"), "extra");
  const unexpected = [...required, ...await collectUploadInputs(root)];
  assert.throws(() => assertUploadBoundary(unexpected), /forbidden upload/);
  await rm(path.join(root, "website", "unexpected.txt"));
  await writeFile(path.join(root, "website", "index.html"), Buffer.alloc(10 * 1024 * 1024));
  const oversized = [...required, ...await collectUploadInputs(root)];
  assert.throws(() => assertUploadBoundary(oversized), /upload exceeds 10 MiB limit/);
});
