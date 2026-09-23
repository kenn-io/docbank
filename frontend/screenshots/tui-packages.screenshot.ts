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
    const records = ["þDOCIDþ\x14þNATIVEþ\x14þBEGBATESþ\x14þENDBATESþ\x14þLABELSETþ"];
    for (let ordinal = 1; ordinal <= 251; ordinal++) {
      const suffix = String(ordinal).padStart(3, "0");
      const document = `DOC-${suffix}`;
      const label = `EXT${String(ordinal).padStart(6, "0")}`;
      records.push(`þ${document}þ\x14þNATIVES/${document}.txtþ\x14þ${label}þ\x14þEXT999999þ\x14þset-${suffix}þ`);
      await writeFile(path.join(volume, "NATIVES", `${document}.txt`),
        `Synthetic package document ${ordinal}\n`, { mode: 0o600 });
    }
    await writeFile(path.join(volume, "DATA", "production.dat"), records.join("\r\n") + "\r\n", { mode: 0o600 });
    const mappingPath = path.join(workspace, "mapping.json");
    await writeFile(mappingPath, JSON.stringify({ contract: "loadfile-mapping/v1", columns: [
      { source: "DOCID", source_ordinal: 0, canonical: "loadfile.document.id" },
      { source: "NATIVE", source_ordinal: 1, canonical: "loadfile.file.native" },
      { source: "BEGBATES", source_ordinal: 2, canonical: "loadfile.label.begin" },
      { source: "ENDBATES", source_ordinal: 3, canonical: "loadfile.label.end" },
      { source: "LABELSET", source_ordinal: 4, canonical: "loadfile.label.set" },
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

  test("shows package status and second-page member and label navigation", async ({ page }) => {
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

    await page.setViewportSize({ width: 1280, height: 760 });
    await page.setContent(`<!doctype html><meta charset="utf-8"><style>
      html,body{margin:0;background:#07100f}.terminal{box-sizing:border-box;width:max-content;min-width:100vw;min-height:100vh;margin:0;padding:16px 18px;color:#e8f1ef;background:#07100f;font:16px/1.25 ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;white-space:pre}
    </style><pre class="terminal"></pre>`);
    const screenshot = async (name: string): Promise<void> => {
      await page.locator(".terminal").evaluate((element, text) => { element.textContent = String(text); }, await capture());
      await page.screenshot({ path: path.join(output, name), fullPage: true, animations: "disabled" });
    };
    await screenshot("tui-packages.png");

    await tmux(["send-keys", "-t", session, "Enter"]);
    await expect.poll(capture).toContain("250+ document(s)");
    await tmux(["send-keys", "-t", session, "n"]);
    await expect.poll(capture).toContain("DOC-251");
    await expect.poll(capture).toContain("1 document(s)");
    await screenshot("tui-package-members-page-2.png");

    await tmux(["send-keys", "-t", session, "Escape"]);
    await expect.poll(capture).toContain("Load-file packages · received and produced");
    await tmux(["send-keys", "-t", session, "l"]);
    await expect.poll(capture).toContain("Label / ");
    await tmux(["send-keys", "-t", session, "EXT999999", "Enter"]);
    await expect.poll(capture).toContain("100+ match(es)");
    await tmux(["send-keys", "-t", session, "n"]);
    await expect.poll(capture).toContain("set-101");
    await tmux(["send-keys", "-t", session, "End"]);
    await expect.poll(capture).toContain("set-200");
    await expect.poll(capture).toContain("100/100");
    await screenshot("tui-package-labels-page-2.png");
  });
});
