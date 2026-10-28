import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import type {
  BatesCandidatePage,
  NumberedReservation,
  RedactionPreparation,
} from "../src/generated/docbank.js";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const captures = process.env.DOCBANK_BATES_RECOVERY_SCREENSHOT_DIR;
test.skip(!captures, "DOCBANK_BATES_RECOVERY_SCREENSHOT_DIR enables retained PDF recovery");

test("recovers a numbered PDF through Productions after a renderer-free restart", async ({
  page,
}) => {
  test.setTimeout(360_000);
  const binary = process.env.DOCBANK_SCREENSHOT_BINARY;
  const inspector = process.env.DOCBANK_BATES_RECOVERY_INSPECTOR;
  if (!binary || !inspector || !path.isAbsolute(binary) || !path.isAbsolute(inspector)) {
    throw new Error("Explicit branch binary and inspector paths are required");
  }
  if (process.platform !== "linux" || process.arch !== "x64")
    throw new Error("This qualification needs Linux amd64");
  await access(binary);
  await access(inspector);
  const scratch =
    process.env.DOCBANK_BATES_RECOVERY_WORKSPACE ??
    (await mkdtemp(path.join(tmpdir(), "docbank-bates-browser-")));
  const vault = path.join(scratch, "vault");
  await mkdir(captures!, { recursive: true, mode: 0o700 });
  const env = { ...process.env, DOCBANK_HOME: vault, DOCBANK_TELEMETRY_ENABLED: "0" };
  const run = async (...args: string[]) =>
    (
      await exec(binary, args, {
        cwd: repository,
        env,
        timeout: 180_000,
        maxBuffer: 16 * 1024 * 1024,
      })
    ).stdout.trim();
  let started = false;
  let failure: unknown;
  const stop = async () => {
    if (!started) return;
    const status = JSON.parse(await run("daemon", "status", "--json"));
    if (status.running) await run("daemon", "stop");
    expect(JSON.parse(await run("daemon", "status", "--json")).running).toBe(false);
    started = false;
  };
  try {
    const renderer =
      process.env.DOCBANK_TEST_PDFTOPPM ?? (await exec("which", ["pdftoppm"])).stdout.trim();
    const limiter = (await exec("which", ["prlimit"])).stdout.trim();
    await exec(
      "go",
      [
        "run",
        "-tags",
        "fts5",
        "./frontend/screenshots/bates-recovery-fixture.go",
        scratch,
        inspector,
        renderer,
        limiter,
      ],
      { cwd: repository, env, timeout: 120_000 },
    );
    started = true;
    await run("add", path.join(scratch, "two-pages.pdf"), "--dest", "/inbox", "--json");
    const preparation = path.join(scratch, "preparation.json");
    const masks = path.join(scratch, "masks.json");
    const review = path.join(scratch, "review.json");
    const prepJSON = await run("redaction", "prepare", "/inbox/two-pages.pdf", "--json");
    const prep: RedactionPreparation = JSON.parse(prepJSON);
    await writeFile(preparation, prepJSON, { mode: 0o600 });
    await writeFile(
      masks,
      JSON.stringify(
        prep.map.pages.map((item, index) => ({
          id: ["11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"][
            index
          ],
          selector: {
            kind: "text",
            map_sha256: prep.map.sha256,
            span: { start: item.span.start, end: item.span.start + 6 },
          },
        })),
      ),
      { mode: 0o600 },
    );
    await writeFile(review, await run("redaction", "resolve", preparation, masks, "--json"), {
      mode: 0o600,
    });
    const namespace = JSON.parse(
      await run("bates", "namespaces", "--create", "--prefix", "QUAL", "--padding", "6", "--json"),
    );
    const reservation: NumberedReservation = JSON.parse(
      await run(
        "redaction",
        "reserve",
        preparation,
        masks,
        review,
        "--namespace",
        namespace.namespace_id,
        "--start",
        "1",
        "--retain-original",
        "--json",
      ),
    );
    expect(reservation.pages.map((item) => item.label)).toEqual(["QUAL000001", "QUAL000002"]);
    const archive = path.join(scratch, "numbered.zip");
    await run(
      "redaction",
      "render",
      preparation,
      masks,
      review,
      archive,
      "--allocation",
      reservation.allocation_id,
      "--json",
    );
    const expectedPath = path.join(scratch, "expected.pdf");
    await exec("python3", [
      "-I",
      "-c",
      "import pathlib,sys,zipfile; pathlib.Path(sys.argv[2]).write_bytes(zipfile.ZipFile(sys.argv[1]).read('redacted.pdf'))",
      archive,
      expectedPath,
    ]);
    const expected = await readFile(expectedPath);
    const expectedHash = createHash("sha256").update(expected).digest("hex");
    await stop();
    await Promise.all([preparation, masks, review].map((file) => rm(file)));
    await writeFile(path.join(vault, "config.toml"), "", { mode: 0o600 });
    started = true;
    const webURL = await run("web", "--no-browser");
    const session = new URLSearchParams(new URL(webURL).hash.slice(1)).get("web_session");
    if (!session) throw new Error("Synthetic browser session missing");
    const pageErrors: string[] = [];
    page.on("pageerror", (error) =>
      pageErrors.push(error.message.replaceAll(session, "[redacted]")),
    );
    await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
    const packageList = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/packages",
    );
    const exportList = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/bates/exports",
    );
    try {
      await page.goto(webURL, { waitUntil: "domcontentloaded" });
    } catch (cause) {
      throw new Error("Could not open synthetic browser session", {
        cause: new Error(String(cause).replaceAll(session, "[redacted]")),
      });
    }
    expect((await (await packageList).json()).items).toHaveLength(0);
    const retained = (await (await exportList).json()).items;
    expect(retained).toHaveLength(1);
    const entry = page.getByRole("button", { name: "Productions", exact: true });
    await expect(entry).toBeVisible();
    await entry.click();
    const drawer = page.getByRole("dialog", { name: "Productions", exact: true });
    await drawer.getByRole("textbox", { name: "Bates number" }).fill("QUAL000002");
    const lookup = page.waitForResponse(
      (response) => new URL(response.url()).pathname === "/api/v1/bates/exports/candidates",
    );
    await drawer.getByRole("textbox", { name: "Bates number" }).press("Enter");
    const response = await lookup;
    expect(new URL(response.url()).searchParams.get("bates_label")).toBe("QUAL000002");
    expect(response.status()).toBe(200);
    const found: BatesCandidatePage = await response.json();
    expect(found.items).toHaveLength(1);
    expect(found.items[0]!.artifact_id).toBe(retained[0].artifact_id);
    expect(found.items[0]!.allocation_id).toBe(reservation.allocation_id);
    expect(found.items[0]!.evidence[0]!.output_page).toBe(2);
    const results = drawer.getByRole("list", { name: "Matching productions" });
    await expect(results.getByText("Page 2 of 2", { exact: true })).toBeVisible();
    await expect(results.locator("details[open]")).toHaveCount(0);
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({
      path: path.join(captures!, "numbered-recovery-collapsed.png"),
      fullPage: true,
      animations: "disabled",
    });
    await results.locator("summary").click();
    await expect(results.getByText("Numbered redaction", { exact: true })).toBeVisible();
    await expect(results.getByText("QUAL", { exact: true })).toBeVisible();
    await expect(results.getByText("None", { exact: true })).toBeVisible();
    await page.screenshot({
      path: path.join(captures!, "numbered-recovery-details.png"),
      fullPage: true,
      animations: "disabled",
    });
    const downloadEvent = page.waitForEvent("download");
    await results.getByRole("button", { name: "Download PDF", exact: true }).click();
    const download = await downloadEvent;
    const downloaded = path.join(scratch, "downloaded.pdf");
    await download.saveAs(downloaded);
    const received = await readFile(downloaded);
    expect(received.length).toBe(expected.length);
    expect(createHash("sha256").update(received).digest("hex")).toBe(expectedHash);
    expect(received.equals(expected)).toBe(true);
    await drawer.getByRole("textbox", { name: "Bates number" }).fill("MISSING000001");
    await drawer.getByRole("button", { name: "Find", exact: true }).click();
    await expect(drawer.getByText("No retained PDF matches “MISSING000001”.")).toBeVisible();
    await page.screenshot({
      path: path.join(captures!, "numbered-recovery-empty.png"),
      fullPage: true,
      animations: "disabled",
    });
    await drawer.getByRole("button", { name: "Clear", exact: true }).click();
    await expect(
      drawer.getByRole("list", { name: "Productions" }).getByRole("listitem"),
    ).toHaveCount(1);
    await drawer.getByRole("button", { name: "Close Productions" }).click();
    await expect(drawer).toBeHidden();
    await expect(entry).toBeFocused();
    expect(pageErrors).toEqual([]);
  } catch (cause) {
    failure = cause;
  }
  try {
    await stop();
  } catch (cause) {
    failure = failure
      ? new AggregateError([failure, cause], "Recovery and shutdown failed")
      : cause;
  }
  if (failure) throw failure;
  await rm(scratch, { recursive: true, force: true });
});
