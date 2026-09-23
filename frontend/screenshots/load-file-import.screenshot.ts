import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
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

function storedZIP(entries: { name: string; bytes: Buffer }[]): Buffer {
  const local: Buffer[] = [], central: Buffer[] = [];
  let offset = 0;
  for (const entry of entries) {
    const name = Buffer.from(entry.name);
    const header = Buffer.alloc(30);
    header.writeUInt32LE(0x04034b50, 0); header.writeUInt16LE(20, 4);
    header.writeUInt32LE(crc32(entry.bytes), 14); header.writeUInt32LE(entry.bytes.length, 18);
    header.writeUInt32LE(entry.bytes.length, 22); header.writeUInt16LE(name.length, 26);
    const directory = Buffer.alloc(46);
    directory.writeUInt32LE(0x02014b50, 0); directory.writeUInt16LE(20, 4); directory.writeUInt16LE(20, 6);
    directory.writeUInt32LE(crc32(entry.bytes), 16); directory.writeUInt32LE(entry.bytes.length, 20);
    directory.writeUInt32LE(entry.bytes.length, 24); directory.writeUInt16LE(name.length, 28);
    directory.writeUInt32LE(offset, 42);
    local.push(header, name, entry.bytes); central.push(directory, name);
    offset += header.length + name.length + entry.bytes.length;
  }
  const centralBytes = Buffer.concat(central);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0); end.writeUInt16LE(entries.length, 8); end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(centralBytes.length, 12); end.writeUInt32LE(offset, 16);
  return Buffer.concat([...local, centralBytes, end]);
}

test.describe("load-file import screenshot", () => {
  let workspace = "", vault = "", webURL = "", zipPath = "";
  async function docbank(args: string[]): Promise<string> {
    return (await run(binary, args, { cwd: repository, env: { ...process.env, DOCBANK_HOME: vault }, timeout: 60_000 })).stdout.trim();
  }
  test.beforeAll(async () => {
    workspace = await mkdtemp(path.join(tmpdir(), "docbank-loadfile-screenshot-"));
    vault = path.join(workspace, "vault"); zipPath = path.join(workspace, "Synthetic production.zip");
    await mkdir(output, { recursive: true, mode: 0o700 });
    const pdf = await readFile(path.join(repository, "document/testdata/scanassessment/blank.pdf"));
    const archive = storedZIP([
      { name: "VOL001/DATA/production.dat", bytes: Buffer.from("þDOCIDþ\x14þNATIVEþ\x14þTEXTþ\x14þBEGBATESþ\r\nþDOC-Aþ\x14þNATIVES/DOC-A.pdfþ\x14þTEXT/DOC-A.txtþ\x14þEXT000001þ\r\n") },
      { name: "VOL001/DATA/production.opt", bytes: Buffer.from("DOC-A,VOL001,IMAGES/DOC-A.tif,Y,,,1\r\n") },
      { name: "VOL001/NATIVES/DOC-A.pdf", bytes: pdf },
      { name: "VOL001/TEXT/DOC-A.txt", bytes: Buffer.from("quenchwood synthetic review text\n") },
      { name: "VOL001/IMAGES/DOC-A.tif", bytes: Buffer.from("synthetic-image") },
    ]);
    await writeFile(zipPath, archive, { mode: 0o600 });
    webURL = await docbank(["web", "--no-browser"]);
  });
  test.afterAll(async () => {
    if (vault) { await docbank(["daemon", "stop"]); const status = JSON.parse(await docbank(["daemon", "status", "--json"])); if (status.running) throw new Error(`daemon still running; retained ${workspace}`); }
    if (workspace) { await rm(workspace, { recursive: true, force: true }); await expect(stat(workspace)).rejects.toMatchObject({ code: "ENOENT" }); }
  });
  test("reviewed ZIP imports with supplied text", async ({ page }) => {
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Import load files", exact: true }).click();
    await page.getByLabel("Choose load-file ZIP", { exact: true }).setInputFiles(zipPath);
    await page.getByRole("button", { name: "Upload and preview", exact: true }).click();
    await expect(page.getByText("Package preview", { exact: true })).toBeVisible();
    await expect(page.getByText("Ready", { exact: true })).toBeVisible();
    await expect(page.getByText("1", { exact: true }).first()).toBeVisible();
    await page.screenshot({ path: path.join(output, "web-load-file-import.png"), fullPage: true });
    await page.getByRole("button", { name: "Import package", exact: true }).click();
    await expect(page.getByText("Import complete", { exact: true })).toBeVisible();
  });
});
