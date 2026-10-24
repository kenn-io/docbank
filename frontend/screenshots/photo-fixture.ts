import { execFile } from "node:child_process";
import { rm } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
export const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");

export function photoFixture(workspace: string, binaryOverride?: string) {
  const binary =
    binaryOverride ??
    process.env.DOCBANK_SCREENSHOT_BINARY ??
    path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");
  const vault = path.join(workspace, "vault");
  const env = {
    ...process.env,
    DOCBANK_HOME: vault,
    DOCBANK_LOCK_DIR: path.join(workspace, "locks"),
  };
  const run = async (...args: string[]) =>
    (await exec(binary, args, { cwd: repository, env, timeout: 60_000 })).stdout.trim();
  const seed = (count = 10_000, ...flags: string[]) =>
    exec(
      "go",
      [
        "run",
        "-tags",
        "fts5",
        "./frontend/screenshots/photos-fixture.go",
        ...flags,
        vault,
        String(count),
      ],
      { cwd: repository, env, timeout: 480_000 },
    );
  const dispose = async () => {
    try {
      await run("daemon", "stop");
    } finally {
      await rm(workspace, { recursive: true, force: true });
    }
  };
  return { run, seed, dispose };
}

export async function withPhotoAPI<T>(url: URL, run: () => Promise<T>): Promise<T> {
  const fetcher = globalThis.fetch;
  try {
    globalThis.fetch = (input, init) => {
      const address = new URL(String(input), url.origin);
      const headers = new Headers(init?.headers);
      headers.set("Host", address.host);
      address.hostname = "127.0.0.1";
      return fetcher(address, { ...init, headers });
    };
    return await run();
  } finally {
    globalThis.fetch = fetcher;
  }
}
