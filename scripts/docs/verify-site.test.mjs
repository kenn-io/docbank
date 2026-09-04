import assert from "node:assert/strict";
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";

import { verifySite } from "./verify-site.mjs";


async function write(root, relative, contents) {
  const destination = path.join(root, relative);
  await mkdir(path.dirname(destination), { recursive: true });
  await writeFile(destination, contents);
}


function page({ canonical, body = "", extraHead = "" }) {
  return `<!doctype html>
<html lang="en"><head>
<title>Fixture page</title>
<meta name="description" content="Fixture description.">
<link rel="canonical" href="${canonical}">
<meta property="og:type" content="website">
<meta property="og:title" content="Fixture page">
<meta property="og:description" content="Fixture description.">
<meta property="og:url" content="${canonical}">
<meta property="og:site_name" content="Docbank">
<meta name="twitter:card" content="summary">
<meta name="twitter:title" content="Fixture page">
<meta name="twitter:description" content="Fixture description.">
${extraHead}</head><body><main><h1>Fixture page</h1>${body}</main></body></html>`;
}


async function fixture(t) {
  const root = await mkdtemp(path.join(tmpdir(), "docbank-site-verify-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const site = path.join(root, "site");
  const website = path.join(root, "website");
  const docs = path.join(root, "docs");
  await write(
    site,
    "index.html",
    page({
      canonical: "https://docbank.ai/",
      body: '<a href="/guide/">Guide</a><img src="/assets/generated/capture.png" alt="Synthetic capture">',
    }),
  );
  await write(
    site,
    "guide/index.html",
    page({ canonical: "https://docbank.ai/guide/", body: '<a href="/docs/">Docs</a>' }),
  );
  await write(
    site,
    "docs/index.html",
    page({ canonical: "https://docbank.ai/docs/", body: '<a href="setup/">Setup</a>' }),
  );
  await write(
    site,
    "docs/setup/index.html",
    page({ canonical: "https://docbank.ai/docs/setup/", body: '<a href="../">Overview</a>' }),
  );
  const markdown = {
    "index.md": "# Product\n",
    "guide.md": "# Guide\n",
    "docs/index.md": "# Operating docs\n",
    "docs/setup.md": "# Setup\n",
  };
  for (const [relative, contents] of Object.entries(markdown)) {
    await write(site, relative, contents);
  }
  await write(website, "index.md", markdown["index.md"]);
  await write(website, "guide.md", markdown["guide.md"]);
  await write(docs, "index.md", markdown["docs/index.md"]);
  await write(docs, "setup.md", markdown["docs/setup.md"]);
  await write(site, "assets/generated/capture.png", "png fixture");
  await write(
    site,
    "llms.txt",
    [
      "# Docbank",
      "- [Product](https://docbank.ai/index.md)",
      "- [Guide](https://docbank.ai/guide.md)",
      "- [Docs](https://docbank.ai/docs/index.md)",
      "- [Setup](https://docbank.ai/docs/setup.md)",
      "",
    ].join("\n"),
  );
  return {
    site,
    sources: {
      website,
      docs,
      assets: ["capture.png"],
    },
  };
}


test("accepts complete routes, Markdown peers, links, and assets", async (t) => {
  const input = await fixture(t);
  await verifySite(input);
});


test("accepts the generated 404 page without a canonical peer", async (t) => {
  const input = await fixture(t);
  const notFound = page({
    canonical: "https://docbank.ai/docs/",
    body: '<a href="#__skip">Skip</a>',
  }).replace('<link rel="canonical" href="https://docbank.ai/docs/">', "");
  await write(input.site, "docs/404.html", notFound);

  await verifySite(input);
});


test("rejects a substantive HTML file that is not a directory route", async (t) => {
  const input = await fixture(t);
  await write(
    input.site,
    "docs/orphan.html",
    page({ canonical: "https://docbank.ai/docs/orphan.html" }),
  );

  await assert.rejects(
    () => verifySite(input),
    /docs\/orphan\.html: unsupported substantive HTML route/,
  );
});


test("rejects an HTML route without its Markdown peer", async (t) => {
  const input = await fixture(t);
  await rm(path.join(input.site, "guide.md"));
  await assert.rejects(() => verifySite(input), /guide\/index\.html: missing Markdown peer guide\.md/);
});


test("rejects a stale llms index", async (t) => {
  const input = await fixture(t);
  await write(
    input.site,
    "llms.txt",
    "- [Product](https://docbank.ai/index.md)\n- [Guide](https://docbank.ai/guide.md)\n- [Docs](https://docbank.ai/docs/index.md)\n",
  );
  await assert.rejects(() => verifySite(input), /llms\.txt: missing \/docs\/setup\.md/);
});


test("rejects a broken local link", async (t) => {
  const input = await fixture(t);
  await write(
    input.site,
    "index.html",
    page({ canonical: "https://docbank.ai/", body: '<a href="/missing/">Missing</a>' }),
  );
  await assert.rejects(() => verifySite(input), /index\.html: broken local URL \/missing\//);
});


test("rejects an image that is not served from the site", async (t) => {
  const { site, sources } = await fixture(t);
  await write(
    site,
    "docs/setup/index.html",
    page({
      canonical: "https://docbank.ai/docs/setup/",
      body: '<img src="https://raw.githubusercontent.com/example/capture.png" alt="Remote capture">',
    }),
  );
  await assert.rejects(() => verifySite({ site, sources }), /image is not served from the site/);
});


test("rejects a private publishing path", async (t) => {
  const input = await fixture(t);
  await write(input.site, "internal/plan.md", "private\n");
  await assert.rejects(() => verifySite(input), /publishing boundary leaked internal\/plan\.md/);
});


test("rejects an undeclared generated asset", async (t) => {
  const input = await fixture(t);
  await write(input.site, "assets/generated/extra.png", "extra\n");
  await assert.rejects(() => verifySite(input), /undeclared generated asset: extra\.png/);
});


test("rejects a Markdown peer that differs from source", async (t) => {
  const input = await fixture(t);
  await write(input.site, "docs/setup.md", "changed\n");
  await assert.rejects(() => verifySite(input), /docs\/setup\.md: published Markdown differs from source/);
});


test("rejects remote font hosts", async (t) => {
  const input = await fixture(t);
  await write(
    input.site,
    "index.html",
    page({
      canonical: "https://docbank.ai/",
      extraHead: '<link rel="stylesheet" href="https://fonts.googleapis.com/css2">',
    }),
  );
  await assert.rejects(() => verifySite(input), /index\.html: remote font URL/);
});
