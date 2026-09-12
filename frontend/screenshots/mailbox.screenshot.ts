import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, rm, writeFile, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { crc32 } from "node:zlib";

const run = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const binary = path.join(repository, "docbank");
const output = process.env.DOCBANK_SCREENSHOT_DIR;
if (!output) throw new Error("DOCBANK_SCREENSHOT_DIR is required");

// A synthetic stored ZIP keeps fixture generation local and independently
// binds the central directory and CRC to the exact mailbox source bytes.
function takeout(source: Buffer): Buffer {
  const name = Buffer.from("Takeout/Mail/All mail.mbox");
  const local = Buffer.alloc(30);
  local.writeUInt32LE(0x04034b50, 0);
  local.writeUInt16LE(20, 4);
  local.writeUInt32LE(crc32(source), 14);
  local.writeUInt32LE(source.length, 18);
  local.writeUInt32LE(source.length, 22);
  local.writeUInt16LE(name.length, 26);
  const central = Buffer.alloc(46);
  central.writeUInt32LE(0x02014b50, 0);
  central.writeUInt16LE(20, 4);
  central.writeUInt16LE(20, 6);
  central.writeUInt32LE(crc32(source), 16);
  central.writeUInt32LE(source.length, 20);
  central.writeUInt32LE(source.length, 24);
  central.writeUInt16LE(name.length, 28);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(1, 8);
  end.writeUInt16LE(1, 10);
  end.writeUInt32LE(central.length + name.length, 12);
  end.writeUInt32LE(local.length + name.length + source.length, 16);
  return Buffer.concat([local, name, source, central, name, end]);
}

test.describe("mailbox import screenshot", () => {
  let workspace = "";
  let vault = "";
  let webURL = "";
  let zipPath = "";
  let sourceHash = "";
  let archiveHash = "";

  async function docbank(args: string[]): Promise<string> {
    const result = await run(binary, args, {
      cwd: repository,
      env: { ...process.env, DOCBANK_HOME: vault },
      timeout: 60_000,
      maxBuffer: 4 * 1024 * 1024,
    });
    return result.stdout.trim();
  }

  test.beforeAll(async () => {
    workspace = await mkdtemp(path.join(tmpdir(), "docbank-mailbox-screenshot-"));
    vault = path.join(workspace, "vault");
    zipPath = path.join(workspace, "Synthetic Takeout.zip");
    await mkdir(output, { recursive: true, mode: 0o700 });
    const message =
      "From sender@example.test Sat Sep 12 10:00:00 2026\n" +
      "Subject: Synthetic project update\nMessage-ID: <same@example.test>\n" +
      "X-Gmail-Labels: Inbox,Project\nContent-Type: multipart/mixed; boundary=m\n\n" +
      "--m\nContent-Type: text/plain\n\nSynthetic project update.\n" +
      "--m\nContent-Type: text/csv\nContent-Disposition: attachment; filename=project.csv\n\n" +
      "item,count\nsynthetic,2\n--m--\n";
    const source = Buffer.from(message + message + message);
    const archive = takeout(source);
    sourceHash = createHash("sha256").update(source).digest("hex");
    archiveHash = createHash("sha256").update(archive).digest("hex");
    await writeFile(zipPath, archive, { mode: 0o600 });
    webURL = await docbank(["web", "--no-browser"]);
    const url = new URL(webURL);
    if (
      !/^docbank-[0-9a-f]{32}\.localhost$/.test(url.hostname) ||
      url.protocol !== "http:" ||
      !url.hash
    ) {
      throw new Error("unexpected daemon-issued browser URL");
    }
  });

  test.afterAll(async () => {
    if (vault) {
      await docbank(["daemon", "stop"]);
      const status = JSON.parse(await docbank(["daemon", "status", "--json"])) as {
        running: boolean;
      };
      if (status.running) throw new Error(`daemon still running; retained ${workspace}`);
    }
    if (workspace) {
      await rm(workspace, { recursive: true, force: true });
      await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" });
    }
  });

  test("verified Takeout with distinct repeated occurrences", async ({ page }) => {
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Import mailbox", exact: true }).click();
    const eyebrow = await page.locator(".drawer-heading span").boundingBox();
    const title = await page.locator(".drawer-heading strong").boundingBox();
    if (!eyebrow || !title) throw new Error("missing mailbox heading geometry");
    expect(title.y).toBeGreaterThanOrEqual(eyebrow.y + eyebrow.height);
    await page.getByLabel("Choose MBOX or Takeout ZIP", { exact: true }).setInputFiles(zipPath);
    await page.getByRole("button", { name: "Upload and preview", exact: true }).click();
    await expect(page.getByText("1 mailbox entry · mboxrd")).toBeVisible();
    await page.getByRole("button", { name: "Import messages", exact: true }).click();
    await expect(page.getByText("Import complete", { exact: true })).toBeVisible();
    await expect(page.getByText("Every message occurrence has been scanned.")).toBeVisible();
    const idText = await page.locator(".mailbox small").textContent();
    if (!idText?.startsWith("Import ")) throw new Error("missing import identity");
    const id = idText.slice(7);
    const job = JSON.parse(await docbank(["mailbox", "status", id])) as {
      imported: number;
      rejected: number;
      pending: number;
      canceled: number;
      scanned_tail: boolean;
      container_sha256: string;
    };
    const occurrences = JSON.parse(await docbank(["mailbox", "receipts", id])) as {
      target: { node_id: number; sha256: string; version_id: string };
      location: { entry_sha256: string; labels: string[]; eml_sha256: string };
    }[];
    expect(job).toMatchObject({
      imported: 3,
      rejected: 0,
      pending: 0,
      canceled: 0,
      scanned_tail: true,
      container_sha256: archiveHash,
    });
    expect(occurrences).toHaveLength(3);
    expect(new Set(occurrences.map((o) => o.target.node_id)).size).toBe(3);
    expect(new Set(occurrences.map((o) => o.target.sha256)).size).toBe(1);
    for (const occurrence of occurrences) {
      expect(occurrence.location.entry_sha256).toBe(sourceHash);
      expect(occurrence.location.eml_sha256).toBe(occurrence.target.sha256);
      expect(occurrence.location.labels).toEqual(["Inbox", "Project"]);
    }
    await page.screenshot({ path: path.join(output, "web-mailbox-import.png"), fullPage: true });
    await writeFile(
      path.join(output, "mailbox-proof.json"),
      JSON.stringify({ job, occurrences }, null, 2) + "\n",
      { mode: 0o600 },
    );
  });
});
