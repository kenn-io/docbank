import assert from "node:assert/strict";
import { access, mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { buildSite } from "./build.mjs";


async function write(root, relative, contents) {
  const destination = path.join(root, relative);
  await mkdir(path.dirname(destination), { recursive: true });
  await writeFile(destination, contents);
}


function html(canonical, link = "") {
  return `<!doctype html><html lang="en"><head><title>Docbank</title>
<meta name="description" content="Docbank fixture.">
<link rel="canonical" href="${canonical}">
<meta property="og:type" content="website"><meta property="og:title" content="Docbank">
<meta property="og:description" content="Docbank fixture."><meta property="og:url" content="${canonical}">
<meta property="og:site_name" content="Docbank"><meta name="twitter:card" content="summary">
<meta name="twitter:title" content="Docbank"><meta name="twitter:description" content="Docbank fixture.">
</head><body><main><h1>Docbank</h1>${link}</main></body></html>`;
}


async function repositoryFixture(t) {
  const repoRoot = await mkdtemp(path.join(tmpdir(), "docbank-site-build-"));
  t.after(() => rm(repoRoot, { recursive: true, force: true }));
  await write(repoRoot, "website/index.html", html("https://docbank.ai/", '<a href="/guide/">Guide</a>'));
  await write(repoRoot, "website/index.md", "# Product\n");
  await write(repoRoot, "website/guide/index.html", html("https://docbank.ai/guide/", '<a href="/docs/">Docs</a>'));
  await write(repoRoot, "website/guide.md", "# Guide\n");
  await write(repoRoot, "website/styles/site.css", "body { color: white; }\n");
  await write(repoRoot, "website/scripts/site.js", "export {};\n");
  await write(repoRoot, "website/favicon.svg", '<svg xmlns="http://www.w3.org/2000/svg"/>\n');
  await write(repoRoot, "scripts/install.sh", "#!/bin/sh\n");
  await write(repoRoot, "scripts/install.ps1", "# installer\n");
  await write(repoRoot, "scripts/docs-assets.txt", "capture.png\n");
  await write(repoRoot, "scripts/docs-assets.ref", `${"a".repeat(40)}\n`);
  await write(repoRoot, "docs/index.md", "# Docs\n");
  await write(repoRoot, "docs/setup.md", "# Setup\n");
  await write(
    repoRoot,
    "docs/llms.txt",
    [
      "# Docbank",
      "- [Product](https://docbank.ai/index.md)",
      "- [Guide](https://docbank.ai/guide.md)",
      "- [Docs](https://docbank.ai/docs/index.md)",
      "- [Setup](https://docbank.ai/docs/setup.md)",
      "",
    ].join("\n"),
  );
  return repoRoot;
}


async function syncAssetsFixture({ repoRoot }) {
  await write(repoRoot, `.cache/docs-assets/${"a".repeat(40)}/capture.png`, "png fixture");
}


async function buildDocsFixture({ repoRoot, output }) {
  await write(output, "index.html", html("https://docbank.ai/docs/", '<a href="setup/">Setup</a>'));
  await write(
    output,
    "setup/index.html",
    html(
      "https://docbank.ai/docs/setup/",
      '<a href="../">Docs</a><img src="https://docbank.ai/assets/generated/capture.png" alt="Capture">',
    ),
  );
  await write(output, "index.md", await readFile(path.join(repoRoot, "docs/index.md")));
  await write(output, "setup.md", await readFile(path.join(repoRoot, "docs/setup.md")));
  await write(output, "llms.txt", "redundant docs index\n");
  await write(output, "install.sh", "redundant installer\n");
}


test("assembles all tiers and public assets into one site root", async (t) => {
  const repoRoot = await repositoryFixture(t);
  const output = path.join(repoRoot, "site");

  await buildSite({ repoRoot, output, buildDocs: buildDocsFixture, syncAssets: syncAssetsFixture });

  for (const relative of [
    "index.html",
    "index.md",
    "guide/index.html",
    "guide.md",
    "docs/index.html",
    "docs/index.md",
    "docs/setup/index.html",
    "docs/setup.md",
    "assets/generated/capture.png",
    "install.sh",
    "install.ps1",
    "llms.txt",
  ]) {
    await access(path.join(output, relative));
  }
  await assert.rejects(access(path.join(output, "docs/llms.txt")));
  await assert.rejects(access(path.join(output, "docs/install.sh")));

  const setup = await readFile(path.join(output, "docs/setup/index.html"), "utf8");
  assert.match(setup, /src="\/assets\/generated\/capture\.png"/);
  assert.doesNotMatch(setup, /docbank\.ai\/assets/);
});


test("fails before assembling when the pinned assets are not synced", async (t) => {
  const repoRoot = await repositoryFixture(t);
  const output = path.join(repoRoot, "site");

  await assert.rejects(
    () => buildSite({ repoRoot, output, buildDocs: buildDocsFixture, syncAssets: async () => {} }),
    { code: "ENOENT" },
  );
  await assert.rejects(access(output));
});


test("keeps the previous marked output when verification fails", async (t) => {
  const repoRoot = await repositoryFixture(t);
  const output = path.join(repoRoot, "site");
  await write(output, ".docbank-docs-output", "generated by docbank docs build\n");
  await write(output, "sentinel.txt", "previous output\n");
  await write(repoRoot, "website/index.html", html("https://docbank.ai/", '<a href="/missing/">Missing</a>'));

  await assert.rejects(
    () => buildSite({ repoRoot, output, buildDocs: buildDocsFixture, syncAssets: syncAssetsFixture }),
    /broken local URL/,
  );
  assert.equal(await readFile(path.join(output, "sentinel.txt"), "utf8"), "previous output\n");
});
