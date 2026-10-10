import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
export const output = process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR;
export async function withPhotoFixture(count: number, preview: string, use: (run: (...args: string[]) => Promise<string>) => Promise<void>, rejects = false) {
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "photos-proof-"));
  const vault = path.join(workspace, "vault");
  const env = { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: path.join(workspace, "locks"), DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  try {
    await mkdir(output!, { recursive: true });
    await exec("go", ["run", "-tags", "fts5", "./frontend/screenshots/photos-fixture.go", vault, String(count), ...(rejects ? ["--rejects"] : [])], { cwd: repository, env, timeout: 480_000 });
    await use(run);
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const url = new URL(await run("web", "--no-browser"));
      url.pathname = preview === "hidden" ? "/photos/hidden" : "/photos";
      await writeFile(path.join(output!, `${preview}-preview.json`), JSON.stringify({ url: url.href, workspace }, null, 2));
    } else {
      try { await run("daemon", "stop"); }
      finally { await rm(workspace, { recursive: true, force: true }); }
    }
  }
}
