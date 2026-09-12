import { mkdir } from "node:fs/promises";
import path from "node:path";
import { expect, test } from "@playwright/test";

declare global {
  interface Window { db28Decode:{ hold:boolean; reached:boolean; closed:number; release:() => void } }
}

const screenshots = process.env.DOCBANK_EMAIL_VIEWER_SCREENSHOT_DIR;
const browserURL = process.env.DOCBANK_DB28_BROWSER_URL;
test("MAIL07 offline email reader on a real daemon", async ({ page, context }) => {
  test.skip(!screenshots || !browserURL, "requires the synthetic Go daemon fixture");
  test.setTimeout(300_000);
  page.setDefaultTimeout(15_000);
  const fixture = JSON.parse(process.env.DOCBANK_DB28_FIXTURE!) as { html:{ version_id:string }; plain:{ version_id:string }; partial:{ version_id:string }; tracker:string };
  await mkdir(screenshots!,{ recursive:true,mode:0o700 });
  const unauthorized:string[] = []; const dialogs:string[] = [];
  context.on("request",(request) => {
    const url = request.url();
    if (url.startsWith(fixture.tracker) || !url.startsWith(new URL(browserURL!).origin) && !url.startsWith("data:") && !url.startsWith("blob:")) unauthorized.push(url);
  });
  page.on("dialog",async (dialog) => { dialogs.push(dialog.message()); await dialog.dismiss(); });
  await page.addInitScript(() => localStorage.setItem("docbank-theme","dark"));
  await page.addInitScript(() => {
    const state = { hold:false, reached:false, closed:0, release:() => {} };
    Object.assign(window,{ db28Decode:state });
    const original = window.createImageBitmap.bind(window);
    window.createImageBitmap = (async (source:ImageBitmapSource, options?:ImageBitmapOptions) => {
      const bitmap = await original(source,options);
      if (state.hold) {
        state.hold = false; state.reached = true;
        const close = bitmap.close.bind(bitmap);
        bitmap.close = () => { state.closed++; close(); };
        await new Promise<void>((resolve) => { state.release = resolve; });
      }
      return bitmap;
    }) as typeof createImageBitmap;
  });
  await page.goto(browserURL!);
  const open = async (name:string) => {
    await page.getByRole("cell",{ name,exact:true }).click();
    await page.getByRole("tab",{ name:"Email",exact:true }).click();
  };
  await open("01-atlas.eml");
  const reader = page.getByRole("region",{ name:"Email reader",exact:true });
  await expect(reader.getByText("Project Atlas — briefing",{ exact:true })).toBeVisible();
  const frame = page.frameLocator('iframe[title="Email HTML body of 01-atlas.eml"]');
  await expect(frame.getByRole("heading",{ name:"Project Atlas briefing" })).toBeVisible();
  await expect(frame.getByRole("img",{ name:"Atlas blue banner" })).toBeVisible();
  expect(await frame.getByRole("img",{ name:"Atlas blue banner" }).evaluate((img) => (img as HTMLImageElement).naturalWidth)).toBe(160);
  await expect(frame.getByText(/Ambiguous resource: Missing or ambiguous CID/)).toBeVisible();
  await expect(frame.getByText(/Missing resource: Missing or ambiguous CID/)).toBeVisible();
  await expect(frame.locator("script:not([src]),form,iframe,object,svg,[onclick],[style]")).toHaveCount(0);
  const emailFrame = page.frames().find((f) => f.url() === "about:srcdoc")!;
  expect(await emailFrame.evaluate(() => (window as Window & { senderExecuted?:boolean }).senderExecuted)).toBeUndefined();
  await expect(frame.locator("details")).not.toHaveAttribute("open");
  await frame.getByText("Show quoted text",{ exact:true }).click();
  await expect(frame.locator("blockquote")).toBeVisible();
  await frame.getByText("End of complete HTML body.",{ exact:true }).scrollIntoViewIfNeeded();
  expect(await emailFrame.evaluate(() => window.scrollY)).toBeGreaterThan(500);
  await frame.getByText("Show quoted text",{ exact:true }).focus();
  await page.keyboard.press("Escape");
  await expect(reader.getByRole("button",{ name:"Enter email body · Escape returns here" })).toBeFocused();
  // A wrong nonce and a shell-window forgery cannot move focus out of a
  // selected control. No sender-supplied bridge events are trusted.
  await reader.getByRole("button",{ name:"Show raw headers",exact:true }).click();
  await expect(reader.getByLabel("Raw email headers")).toContainText("X-Trace: first\r\nX-Trace: second");
  await page.evaluate(() => window.postMessage({ channel:"docbank-email",nonce:"old",action:"escape" },location.origin));
  await expect(reader.getByRole("button",{ name:"Hide raw headers",exact:true })).toBeFocused();
  await reader.getByRole("button",{ name:"Hide raw headers",exact:true }).click();
  await frame.getByRole("heading",{ name:"Project Atlas briefing" }).scrollIntoViewIfNeeded();
  await page.setViewportSize({ width:1440,height:2200 });
  await reader.screenshot({ path:path.join(screenshots!,"web-email-html.png"),animations:"disabled" });
  await reader.getByRole("combobox",{ name:/^Email body:/ }).click();
  await page.getByRole("option",{ name:"Plain text · part 1.1.1 · available",exact:true }).click();
  await expect(reader.getByLabel("Plain text email body")).toHaveText("Plain alternative remains complete.");
  await expect(page.locator("iframe")).toHaveCount(0);
  await reader.getByRole("combobox",{ name:/^Email body:/ }).click();
  await page.getByRole("option",{ name:"HTML · part 1.1.2 · available",exact:true }).click();
  await expect(frame.getByRole("heading",{ name:"Project Atlas briefing" })).toBeVisible();
  await reader.getByRole("button",{ name:"Inspect attachment documents",exact:true }).click();
  await page.getByRole("button",{ name:"Open attachment notes.txt, part 1.5",exact:true }).click();
  await expect(page.getByText("Synthetic attachment exact bytes.",{ exact:true })).toBeVisible();
  await page.getByRole("button",{ name:"Return to selected document",exact:true }).click();
  await page.getByRole("tab",{ name:"Email",exact:true }).click();
  await expect(frame.getByRole("heading",{ name:"Project Atlas briefing" })).toBeVisible();
  const pendingDownload = page.waitForEvent("download");
  await page.getByRole("button",{ name:"Download verified original",exact:true }).click();
  const download = await pendingDownload;
  expect(download.suggestedFilename()).toBe("01-atlas.eml");
  await download.delete();
  // Hold the real daemon's body response, navigate, then release it. The old
  // fetch must fail and no prior message frame may mount on the new source.
  await page.getByRole("tab",{ name:"Attachments",exact:true }).click();
  let release!:() => void; let reached!:() => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  const started = new Promise<void>((resolve) => { reached = resolve; });
  await page.route(`**/versions/${fixture.html.version_id}/email/generations/*/parts/*/body_utf8`,async (route) => { reached(); await held; await route.continue(); },{ times:1 });
  await page.getByRole("tab",{ name:"Email",exact:true }).click();
  await started;
  const failed = page.waitForEvent("requestfailed",{ predicate:(request) => request.url().includes(fixture.html.version_id) && request.url().endsWith("/body_utf8") });
  await open("02-plain.eml"); release(); await failed;
  await expect(reader.getByLabel("Plain text email body")).toContainText("Plain text keeps <script> inert.");
  await expect(page.locator('iframe[title="Email HTML body of 01-atlas.eml"]')).toHaveCount(0);
  await expect(reader).toContainText("invalid");
  await reader.getByLabel("Plain text email body").focus();
  await page.keyboard.press("Control+End");
  await expect.poll(() => reader.getByLabel("Plain text email body").evaluate((element) => element.scrollTop)).toBeGreaterThan(100);
  await reader.getByLabel("Plain text email body").evaluate((element) => { element.scrollTop = 0; });
  await reader.screenshot({ path:path.join(screenshots!,"web-email-plain.png"),animations:"disabled" });
  // Delay completion of the actual raster decoder, change the selected
  // source, then release it: the decoded resource must close without mount.
  await page.evaluate(() => { window.db28Decode.hold = true; });
  console.log("MAIL07 starting delayed raster decode");
  await open("01-atlas.eml");
  await expect.poll(() => page.evaluate(() => window.db28Decode.reached)).toBe(true);
  await open("02-plain.eml");
  await page.evaluate(() => { window.db28Decode.release(); });
  await expect.poll(() => page.evaluate(() => window.db28Decode.closed)).toBe(1);
  await expect(page.locator("iframe")).toHaveCount(0);
  console.log("MAIL07 delayed raster closed without publication");
  // Delay the trusted frame asset so its load cannot complete before source
  // navigation. The old frame is removed, including its pending load handler.
  let releaseFrame!:() => void; let frameReached = false;
  const heldFrame = new Promise<void>((resolve) => { releaseFrame = resolve; });
  await page.route("**/assets/email-frame-bridge-*.js",async (route) => { frameReached = true; await heldFrame; await route.continue(); },{ times:1 });
  await open("01-atlas.eml"); await expect.poll(() => frameReached).toBe(true);
  await open("02-plain.eml"); releaseFrame();
  await expect(reader.getByLabel("Plain text email body")).toContainText("End of complete plain body.");
  await expect(page.locator("iframe")).toHaveCount(0);
  await open("03-partial.eml");
  await expect(reader).toContainText("MIME inventory: partial");
  await expect(reader.getByLabel("Plain text email body")).toContainText("readable body survives");
  await reader.getByText(/^MIME warnings/).click();
  await reader.screenshot({ path:path.join(screenshots!,"web-email-partial.png"),animations:"disabled" });
  // Corrupt a real part receipt without replacing its bytes: identity failure
  // must become an unavailable image, never a best-effort CID match.
  await page.route(`**/versions/${fixture.html.version_id}/email/generations/*/parts/1.2/decoded_payload`,async (route) => {
    const response = await route.fetch();
    await route.fulfill({ response,headers:{ ...response.headers(),"x-docbank-email-part-path":"1.3" } });
  },{ times:1 });
  await open("01-atlas.eml");
  await expect(frame.getByText(/Atlas blue banner: Email part response identity mismatch/)).toBeVisible();
  await expect(frame.getByRole("img",{ name:"Atlas blue banner" })).toHaveCount(0);
  await page.getByRole("tab",{ name:"Attachments",exact:true }).click();
  await page.route(`**/versions/${fixture.html.version_id}/email`,async (route) => {
    const response = await route.fetch(); const body = await response.json();
    body.generation_id = "0".repeat(64);
    await route.fulfill({ response,json:body });
  },{ times:1 });
  await page.getByRole("tab",{ name:"Email",exact:true }).click();
  await expect(reader).toContainText("canonical evidence or generation binding failed verification");
  await expect(page.locator("iframe")).toHaveCount(0);
  expect(unauthorized).toEqual([]); expect(dialogs).toEqual([]);
  console.log("MAIL07: zero unauthorized browser requests and sender dialogs; CID bytes/dimensions, missing/ambiguous placeholders, quotes, long scrolling, Escape, raw headers, exact original, attachment navigation and stale fetch/decode/frame-load cancellation verified.");
});
