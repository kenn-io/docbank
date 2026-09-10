import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmod, copyFile, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repositoryRoot = fileURLToPath(new URL("../../", import.meta.url));

test("production deployment checks its upload report before uploading", async (t) => {
  const scratch = await mkdtemp(path.join(tmpdir(), "docbank-deploy-test-"));
  t.after(() => rm(scratch, { recursive: true, force: true }));
  const repo = path.join(scratch, "repo");
  const remote = path.join(scratch, "remote.git");
  const bin = path.join(scratch, "bin");
  await mkdir(path.join(repo, "scripts", "docs"), { recursive: true });
  await mkdir(bin);
  const env = Object.fromEntries(Object.entries(process.env).filter(
    ([name]) => !name.startsWith("GIT_") && !name.startsWith("VERCEL_"),
  ));
  Object.assign(env, {
    GIT_CONFIG_GLOBAL: path.join(scratch, "gitconfig"),
    GIT_CONFIG_NOSYSTEM: "1",
    PATH: `${bin}${path.delimiter}${process.env.PATH}`,
    VERCEL_ORG_ID: "fixture-team",
    VERCEL_PROJECT_ID: "fixture-project",
    DEPLOY_CALLS: path.join(scratch, "calls.jsonl"),
    UPLOAD_REPORT: path.join(scratch, "report.json"),
  });
  const git = (...args) => {
    const result = spawnSync("git", args, { cwd: repo, env, encoding: "utf8" });
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.trim();
  };
  for (const relative of [
    "scripts/deploy-docs.sh",
    "scripts/validate-docs-release.sh",
    "scripts/docs/assert-vercel-dry-run.mjs",
  ]) {
    await copyFile(path.join(repositoryRoot, relative), path.join(repo, relative));
  }
  const files = [
    "vercel.json", "website/index.html", "docs/zensical.toml", "docs/uv.lock",
    "scripts/vercel-install-docs.sh", "scripts/vercel-build-docs.sh",
    "scripts/sync-docs-assets.sh", "scripts/docs-assets.ref", "scripts/docs-assets.txt",
    "scripts/docs/build.mjs", "scripts/docs/verify-site.mjs", "scripts/install.sh", "scripts/install.ps1",
  ].map((entry) => ({ path: entry, size: 1 }));
  for (const { path: relative } of files) {
    await mkdir(path.dirname(path.join(repo, relative)), { recursive: true });
    await writeFile(path.join(repo, relative), "fixture\n");
  }
  const extra = "website/assets/operator-notes.txt";
  await mkdir(path.dirname(path.join(repo, extra)), { recursive: true });
  await writeFile(path.join(repo, extra), "Synthetic non-publication input.\n");
  git("init", "--quiet", "--bare", remote);
  git("init", "--quiet", "-b", "main");
  git("config", "user.name", "Deployment Test");
  git("config", "user.email", "deploy-test@example.invalid");
  git("add", ".");
  git("commit", "--quiet", "-m", "release fixture");
  git("tag", "v1.0.0");
  git("remote", "add", "origin", remote);
  git("push", "--quiet", "origin", "main", "--tags");
  env.DOCS_SOURCE = git("rev-parse", "HEAD");
  await writeFile(path.join(bin, "vercel"), `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.DEPLOY_CALLS, JSON.stringify(args) + "\\n");
if (args.includes("--dry")) {
  process.stdout.write(fs.readFileSync(process.env.UPLOAD_REPORT));
  process.exitCode = Number(process.env.DRY_EXIT || 0);
} else if (args[0] === "deploy") {
  console.log("https://fixture.vercel.app");
} else if (args[0] === "inspect" && process.env.CANCEL_INSPECT === "1") {
  process.kill(process.ppid, "SIGTERM");
}
`);
  await chmod(path.join(bin, "vercel"), 0o755);

  for (const scenario of ["extra-file", "dry-run-failure", "cancel-inspect", "allowed"]) {
    await t.test(scenario, async () => {
      await writeFile(env.DEPLOY_CALLS, "");
      const reported = scenario === "extra-file" ? [...files, { path: extra, size: 40 }] : files;
      await writeFile(env.UPLOAD_REPORT, JSON.stringify({ files: reported }));
      const result = spawnSync("sh", ["scripts/deploy-docs.sh"], {
        cwd: repo, env: {
          ...env,
          DRY_EXIT: scenario === "dry-run-failure" ? "1" : "0",
          CANCEL_INSPECT: scenario === "cancel-inspect" ? "1" : "0",
        },
        encoding: "utf8", timeout: 30_000,
      });
      const calls = (await readFile(env.DEPLOY_CALLS, "utf8")).trim().split("\n").map(JSON.parse);
      assert.ok(calls[0].includes("--dry"), "first Vercel call must be a dry run");
      assert.ok(calls[0].includes("--prod"), "check the production upload inputs");
      assert.ok(calls[0].includes("--json"), "validate the structured upload report");
      if (scenario === "allowed") {
        assert.equal(result.status, 0, result.stderr);
        assert.deepEqual(calls.slice(1).map(([command]) => command), ["deploy", "inspect", "promote"]);
      } else if (scenario === "cancel-inspect") {
        assert.notEqual(result.status, 0, "termination must stop deployment");
        assert.deepEqual(calls.slice(1).map(([command]) => command), ["deploy", "inspect"]);
      } else {
        assert.notEqual(result.status, 0);
        assert.equal(calls.length, 1, "rejected inputs must never be uploaded or promoted");
        if (scenario === "extra-file") assert.match(result.stderr, /forbidden upload: website\/assets\/operator-notes\.txt/);
      }
    });
  }
});
