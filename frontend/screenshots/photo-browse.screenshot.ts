import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { build } from "vite";
import type { PhotoBrowsePage, PhotoPreviewSlot } from "../src/generated/docbank";
import type { PhotoPreviewCache, PhotoPreviewLease } from "../src/photoPreviews";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const output = process.env.DOCBANK_PHOTO_BROWSE_SCREENSHOT_DIR;
test.skip(!output, "DOCBANK_PHOTO_BROWSE_SCREENSHOT_DIR enables the focused photo module proof");

declare global {
  interface Window {
    PhotoBrowseProof: {
      PhotoPreviewCache: typeof PhotoPreviewCache;
      listPhotoAssets(body: { query: object }, options: { session: string }): Promise<PhotoBrowsePage>;
    };
    photoProofCache?: PhotoPreviewCache;
    photoProofLeases?: PhotoPreviewLease[];
    photoProofRevocations?: number;
  }
}

test("photo previews decode through the production cache and header transport", async ({ page }) => {
  const scratch = await mkdtemp(path.join(tmpdir(), "docbank-photo-browse-proof-"));
  const vault = path.join(scratch, "vault");
  const source = path.join(scratch, "camera");
  const entry = path.join(repository, ".superpowers", "photo-browse", "entry.ts");
  const binary = process.env.DOCBANK_SCREENSHOT_BINARY ?? path.join(repository, ".superpowers", "photo-browse", process.platform === "win32" ? "docbank.exe" : "docbank");
  const run = async (...args: string[]) => (await exec(binary, args, { cwd: repository, env: { ...process.env, DOCBANK_HOME: vault, DOCBANK_LOCK_DIR: path.join(scratch, "locks") }, timeout: 60_000 })).stdout.trim();
  let running = false;
  try {
    await mkdir(source, { recursive: true, mode: 0o700 });
    await mkdir(path.dirname(entry), { recursive: true });
    await writeFile(entry, 'export { PhotoPreviewCache } from "../../frontend/src/photoPreviews.ts";\nexport { listPhotoAssets } from "../../frontend/src/generated/docbank.ts";\n');
    const result = await build({ root: path.join(repository, "frontend"), configFile: false, logLevel: "error", build: { write: false, minify: false, lib: { entry, name: "PhotoBrowseProof", formats: ["iife"] } } });
    const bundles = Array.isArray(result) ? result : [result];
    const chunk = bundles.flatMap((bundle) => "output" in bundle ? bundle.output : []).find((item) => item.type === "chunk");
    if (!chunk || chunk.type !== "chunk") throw new Error("Production photo preview bundle missing");
    const png = await page.evaluate(() => {
      const canvas = document.createElement("canvas"); canvas.width = 640; canvas.height = 360;
      const ctx = canvas.getContext("2d")!;
      ctx.fillStyle = "#163f55"; ctx.fillRect(0, 0, 640, 360);
      ctx.fillStyle = "#3c7961"; ctx.beginPath(); ctx.moveTo(0, 360); ctx.lineTo(180, 90); ctx.lineTo(390, 360); ctx.fill();
      ctx.fillStyle = "#65ac80"; ctx.beginPath(); ctx.moveTo(210, 360); ctx.lineTo(480, 80); ctx.lineTo(640, 360); ctx.fill();
      ctx.fillStyle = "#e6c15b"; ctx.beginPath(); ctx.arc(535, 65, 30, 0, Math.PI * 2); ctx.fill();
      return canvas.toDataURL("image/png").split(",")[1]!;
    });
    await writeFile(path.join(source, "synthetic-landscape.png"), Buffer.from(png, "base64"), { mode: 0o600 });
    const webURL = await run("web", "--no-browser"); running = true;
    const url = new URL(webURL);
    const session = new URLSearchParams(url.hash.slice(1)).get("web_session");
    if (!session) throw new Error("Synthetic daemon did not issue a browser session");
    await run("photos", "import", source, "/Photos/Synthetic");
    const requests: { path: string; header: boolean; credentialInURL: boolean }[] = [];
    page.on("request", (request) => {
      if (request.url().includes("/photos/assets/")) requests.push({ path: new URL(request.url()).pathname, header: request.headers()["x-docbank-web-session"] === session, credentialInURL: request.url().includes(session) });
    });
    await page.goto(webURL, { waitUntil: "domcontentloaded" });
    await page.route("**/photo-browse-proof.js", (route) => route.fulfill({ contentType: "text/javascript", body: chunk.code }));
    await page.addScriptTag({ url: new URL("/photo-browse-proof.js", webURL).href });
    let slot: PhotoPreviewSlot | undefined;
    await expect.poll(async () => {
      slot = await page.evaluate(async (token) => {
        const result = await window.PhotoBrowseProof.listPhotoAssets({ query: { filters: { kinds: ["photo"] } } }, { session: token });
        return result.items[0]?.previews.grid;
      }, session);
      return slot?.state;
    }, { timeout: 60_000 }).toBe("ready");
    const decoded = await page.evaluate(async ({ token, preview }) => {
      const cache = new window.PhotoBrowseProof.PhotoPreviewCache(token);
      window.photoProofCache = cache;
      const revoke = URL.revokeObjectURL.bind(URL); window.photoProofRevocations = 0;
      URL.revokeObjectURL = (url) => { window.photoProofRevocations!++; revoke(url); };
      const controller = new AbortController();
      const leases = await Promise.all([cache.acquire(preview, controller.signal), cache.acquire(preview, controller.signal)]);
      window.photoProofLeases = leases;
      const section = document.createElement("section");
      section.style.cssText = "max-width:680px;margin:48px auto;padding:24px;background:#171b21;border-radius:12px;color:#e7e9ee;font:16px system-ui;box-sizing:border-box";
      const title = document.createElement("h1"); title.textContent = "Photo preview transport";
      const description = document.createElement("p"); description.textContent = "Verified JPEG from the synthetic vault, displayed through an authenticated object URL.";
      const image = document.createElement("img"); image.alt = "Synthetic green mountains beneath a yellow sun on a blue background"; image.style.cssText = "width:100%;height:auto;display:block;border-radius:6px"; image.src = leases[0]!.url;
      const caption = document.createElement("p"); const italic = document.createElement("em"); italic.textContent = "Synthetic data. Production cache and daemon transport proof."; caption.append(italic);
      section.append(title, description, image, caption); document.body.replaceChildren(section); document.body.style.cssText = "margin:0;padding:0 16px;background:#0e1116";
      await image.decode();
      return { width: image.naturalWidth, height: image.naturalHeight, objectURL: image.src.startsWith("blob:"), sameLeaseURL: leases[0]!.url === leases[1]!.url };
    }, { token: session, preview: slot! });
    expect(decoded).toEqual({ width: 512, height: 288, objectURL: true, sameLeaseURL: true });
    expect(requests.every((request) => request.header && !request.credentialInURL)).toBe(true);
    expect(requests.filter((request) => request.path.includes("/previews/"))).toHaveLength(1);
    await mkdir(output!, { recursive: true });
    for (const width of [1440, 1280, 768, 400]) {
      await page.setViewportSize({ width, height: 960 });
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      await page.screenshot({ path: path.join(output!, `photo-preview-${width}-dark.png`) });
    }
    const revoked = await page.evaluate(() => { for (const lease of window.photoProofLeases!) lease.release(); window.photoProofCache!.dispose(); return window.photoProofRevocations; });
    expect(revoked).toBe(1);
    await writeFile(path.join(output!, "browser-evidence.json"), JSON.stringify({ decoded, requests, revoked, widths: [1440, 1280, 768, 400], synthetic: true }, null, 2));
  } finally {
    if (running) await run("daemon", "stop");
    await rm(scratch, { recursive: true, force: true });
    await rm(entry, { force: true });
  }
});
