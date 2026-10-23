import { spawnSync } from "node:child_process";
import { copyFile, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { publishScreenshots } from "./publish.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const frontendRoot = path.resolve(here, "..");
const repositoryRoot = path.resolve(frontendRoot, "..");
const workspaceRoot = path.join(repositoryRoot, ".superpowers");
const output = path.join(workspaceRoot, "screenshots");
const staging = path.join(workspaceRoot, ".screenshots.next");
const captures = path.join(workspaceRoot, ".screenshots.capture");
const manifestPath = path.join(repositoryRoot, "scripts", "docs-assets.txt");

const build = spawnSync("make", ["build"], {
  cwd: repositoryRoot,
  stdio: "inherit",
});
if (build.error) throw build.error;
if (build.status !== 0) process.exit(build.status ?? 1);

await rm(staging, { recursive: true, force: true });
await rm(captures, { recursive: true, force: true });
await mkdir(staging, { recursive: true, mode: 0o700 });
await mkdir(captures, { recursive: true, mode: 0o700 });
// Some screens show the absolute path of a seeded import folder. The default
// temporary directory on macOS is per-user (/var/folders/...), so captures
// use a fresh directory under /tmp instead.
const scratch = await mkdtemp(
  path.join(process.platform === "win32" ? tmpdir() : "/tmp", "docbank-screenshots-"),
);

const playwrightCLI = path.join(frontendRoot, "node_modules", "@playwright", "test", "cli.js");
const playwright = (args, env) =>
  spawnSync(
    process.execPath,
    [
      playwrightCLI,
      "test",
      ...args,
      "--config",
      path.join(here, "playwright.config.ts"),
      "--project",
      "chromium",
    ],
    {
      cwd: frontendRoot,
      stdio: "inherit",
      env: { ...process.env, TMPDIR: scratch, TMP: scratch, TEMP: scratch, ...env },
    },
  );
const check = async (result) => {
  if (result.error) throw result.error;
  if (result.status === 0) return;
  await rm(scratch, { recursive: true, force: true });
  await rm(staging, { recursive: true, force: true });
  process.exit(result.status ?? 1);
};

await check(
  playwright(process.argv.slice(2), {
    DOCBANK_SCREENSHOT_DIR: captures,
    DOCBANK_QUERY_BAR_SCREENSHOT_DIR: captures,
    DOCBANK_TERM_REPORT_SCREENSHOT_DIR: captures,
    DOCBANK_BATES_SCREENSHOT_DIR: captures,
    DOCBANK_EXPORT_SCREENSHOT_DIR: captures,
    DOCBANK_NATURAL_SEARCH_SCREENSHOT_DIR: captures,
    DOCBANK_SIMILAR_SCREENSHOT_DIR: captures,
    DOCBANK_SNAPSHOT_SCREENSHOT_DIR: captures,
    DOCBANK_SIGNIN_SCREENSHOT_DIR: captures,
  }),
);

// The map needs a binary that trusts the spec's synthetic tile server, so it
// runs separately against that build.
const mapBinary = path.join(
  repositoryRoot,
  "bin",
  process.platform === "win32" ? "docbank-map.exe" : "docbank-map",
);
await check(
  spawnSync("go", ["build", "-tags", "fts5 screenshot_tiles", "-o", mapBinary, "./cmd/docbank"], {
    cwd: repositoryRoot,
    stdio: "inherit",
  }),
);
await check(
  playwright(["photo-map.screenshot.ts"], {
    DOCBANK_SCREENSHOT_BINARY: mapBinary,
    DOCBANK_MAP_PROOF_DIR: captures,
  }),
);
await rm(scratch, { recursive: true, force: true });

const names = (await readFile(manifestPath, "utf8")).split("\n").filter((line) => line !== "");
// Proof runs also write receipts and alternate viewport captures. Only the
// complete public image manifest enters the atomically published set.
for (const name of names) {
  await copyFile(path.join(captures, name), path.join(staging, name));
}
await publishScreenshots({ output, staging, names });
await rm(captures, { recursive: true, force: true });
