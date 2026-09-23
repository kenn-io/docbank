import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { mkdir, mkdtemp, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const run = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(repository, process.platform === "win32" ? "docbank.exe" : "docbank");
const output = process.env.DOCBANK_SCREENSHOT_DIR;
if (!output) throw new Error("DOCBANK_SCREENSHOT_DIR is required");

test.describe("TUI packages screenshot", () => {
  let workspace = "";
  let vault = "";
  let socket = "";

  async function docbank(args: string[]): Promise<string> {
    return (await run(binary, args, {
      cwd: repository,
      env: { ...process.env, DOCBANK_HOME: vault },
      timeout: 60_000,
    })).stdout.trim();
  }

  test.beforeAll(async () => {
    workspace = await mkdtemp(path.join(tmpdir(), "docbank-tui-packages-"));
    vault = path.join(workspace, "vault");
    const volume = path.join(workspace, "package", "VOL001");
    await mkdir(path.join(volume, "DATA"), { recursive: true, mode: 0o700 });
    await mkdir(path.join(volume, "NATIVES"), { recursive: true, mode: 0o700 });
    await mkdir(output, { recursive: true, mode: 0o700 });
    await writeFile(path.join(volume, "DATA", "production.dat"),
      "þDOCIDþ\x14þNATIVEþ\x14þBEGBATESþ\r\nþDOC-Aþ\x14þNATIVES/DOC-A.txtþ\x14þEXT000001þ\r\n", { mode: 0o600 });
    await writeFile(path.join(volume, "NATIVES", "DOC-A.txt"), "synthetic package document\n", { mode: 0o600 });
    const mappingPath = path.join(workspace, "mapping.json");
    await writeFile(mappingPath, JSON.stringify({ contract: "loadfile-mapping/v1", columns: [
      { source: "DOCID", source_ordinal: 0, canonical: "loadfile.document.id" },
      { source: "NATIVE", source_ordinal: 1, canonical: "loadfile.file.native" },
      { source: "BEGBATES", source_ordinal: 2, canonical: "loadfile.label.begin" },
    ] }), { mode: 0o600 });
    await docbank(["web", "--no-browser"]);
    const preflight = JSON.parse(await docbank(["package", "preflight", path.join(workspace, "package"),
      "--profile", "dat-concordance-v1", "--encoding", "utf-8", "--map", mappingPath, "--json"]));
    const operationID = randomUUID();
    await docbank(["package", "import", preflight.preflight_id, "--name", "synthetic-production",
      "--operation-id", operationID, "--json"]);
    await expect.poll(async () => JSON.parse(await docbank(["package", "import", "status", operationID, "--json"])).state,
      { timeout: 30_000 }).toBe("complete");
  });

  test.afterAll(async () => {
    if (socket) await run("tmux", ["-L", socket, "kill-server"]).catch(() => undefined);
    if (vault) {
      await docbank(["daemon", "stop"]);
      const status = JSON.parse(await docbank(["daemon", "status", "--json"]));
      if (status.running) throw new Error(`daemon still running; retained ${workspace}`);
    }
    if (workspace) {
      await rm(workspace, { recursive: true, force: true });
      await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" });
    }
  });

  test("shows retained package status and controls", async ({ page }) => {
    socket = `docbank-packages-${process.pid}`;
    const session = "docbank-tui";
    const tmux = async (args: string[]): Promise<string> => (await run("tmux", ["-L", socket, ...args], {
      cwd: repository,
      env: { ...process.env, DOCBANK_HOME: vault, DOCBANK_SCREENSHOT_BINARY: binary, TERM: "xterm-256color" },
      timeout: 15_000,
    })).stdout;
    const capture = async (): Promise<string> => tmux(["capture-pane", "-p", "-t", session, "-S", "0"]);
    await tmux(["new-session", "-d", "-x", "120", "-y", "38", "-s", session, 'exec "$DOCBANK_SCREENSHOT_BINARY" tui']);
    await expect.poll(capture).toContain("documents for you and your agents");
    await tmux(["send-keys", "-t", session, "K"]);
    await expect.poll(capture).toContain("synthetic-production");
    const terminal = await capture();

    await page.setViewportSize({ width: 1280, height: 760 });
    await page.setContent(`<!doctype html><meta charset="utf-8"><style>
      html,body{margin:0;background:#07100f}.terminal{box-sizing:border-box;width:max-content;min-width:100vw;min-height:100vh;margin:0;padding:16px 18px;color:#e8f1ef;background:#07100f;font:16px/1.25 ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;white-space:pre}
    </style><pre class="terminal"></pre>`);
    await page.locator(".terminal").evaluate((element, text) => { element.textContent = String(text); }, terminal);
    await page.screenshot({ path: path.join(output, "tui-packages.png"), fullPage: true, animations: "disabled" });
  });
});
