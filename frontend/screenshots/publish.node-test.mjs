import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { publishScreenshots } from "./publish.mjs";

// This suite uses Node's built-in test runner and is intentionally named so
// Vitest does not discover it as part of the Svelte unit-test lane.


const pngBytes = Buffer.from([
  0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00,
]);


async function workspace(t) {
  const root = await mkdtemp(path.join(tmpdir(), "docbank-screenshot-publish-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  return {
    output: path.join(root, "screenshots"),
    staging: path.join(root, ".screenshots.next"),
  };
}


test("publishes a complete staged set atomically", async (t) => {
  const { output, staging } = await workspace(t);
  await mkdir(output);
  await writeFile(path.join(output, "old.png"), pngBytes);
  await mkdir(staging);
  await writeFile(path.join(staging, "one.png"), pngBytes);

  await publishScreenshots({ output, staging, names: ["one.png"] });

  assert.deepEqual(await readdir(output), ["one.png"]);
  assert.deepEqual(await readFile(path.join(output, "one.png")), pngBytes);
});


test("keeps the previous set when the staged manifest is incomplete", async (t) => {
  const { output, staging } = await workspace(t);
  await mkdir(output);
  await writeFile(path.join(output, "old.png"), pngBytes);
  await mkdir(staging);
  await writeFile(path.join(staging, "one.png"), pngBytes);

  await assert.rejects(
    publishScreenshots({
      output,
      staging,
      names: ["one.png", "two.png"],
    }),
    /missing screenshot: two\.png/,
  );

  assert.deepEqual(await readdir(output), ["old.png"]);
  assert.deepEqual(await readFile(path.join(output, "old.png")), pngBytes);
});


test("restores the previous set when final rename fails", async (t) => {
  const { output, staging } = await workspace(t);
  await mkdir(output);
  await writeFile(path.join(output, "old.png"), pngBytes);
  await mkdir(staging);
  await writeFile(path.join(staging, "one.png"), pngBytes);
  let renameCalls = 0;

  await assert.rejects(
    publishScreenshots({
      output,
      staging,
      names: ["one.png"],
      async rename(from, to, realRename) {
        renameCalls += 1;
        if (renameCalls === 2) throw new Error("synthetic rename failure");
        await realRename(from, to);
      },
    }),
    /synthetic rename failure/,
  );

  assert.deepEqual(await readdir(output), ["old.png"]);
  assert.deepEqual(await readFile(path.join(output, "old.png")), pngBytes);
});


test("rejects unsafe manifest names before changing output", async (t) => {
  const { output, staging } = await workspace(t);
  await mkdir(output);
  await writeFile(path.join(output, "old.png"), pngBytes);
  await mkdir(staging);

  await assert.rejects(
    publishScreenshots({ output, staging, names: ["nested/one.png"] }),
    /invalid screenshot name/,
  );

  assert.deepEqual(await readdir(output), ["old.png"]);
});
