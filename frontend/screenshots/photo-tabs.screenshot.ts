import { test, expect, type Page, type Request } from "@playwright/test";
import { mkdtemp } from "node:fs/promises";
import path from "node:path";
import { getListPhotoAlbumsUrl } from "../src/generated/docbank.js";
import { photoFixture, repository } from "./photo-fixture.js";

test.skip(
  !process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR,
  "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof",
);

test("six visible tabs retain HTTP/1.1 request capacity", async ({ context }) => {
  test.setTimeout(180_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "six-tabs-proof-"));
  const fixture = photoFixture(workspace);
  const tabs: { page: Page; session: string }[] = [];
  const streams = new Set<Request>();
  try {
    await fixture.seed(6);
    for (let index = 0; index < 6; index++) {
      const url = new URL(await fixture.run("web", "--no-browser"));
      const session = new URLSearchParams(url.hash.slice(1)).get("web_session");
      expect(session).toBeTruthy();
      expect(tabs.some((tab) => tab.session === session)).toBe(false);
      url.pathname = "/photos";
      const page = await context.newPage();
      tabs.push({ page, session: session! });
      page.on("response", (response) => {
        if (
          new URL(response.url()).pathname === "/api/v1/photos/events" &&
          response.status() === 200
        ) {
          streams.add(response.request());
          console.log("Active photo event streams:", streams.size);
        }
      });
      page.on("requestfinished", (request) => streams.delete(request));
      page.on("requestfailed", (request) => streams.delete(request));
      await page.goto(url.href, { waitUntil: "domcontentloaded", timeout: 20_000 });
      await expect(page.locator("[data-asset]"), `Photo list in tab ${index + 1}`).toHaveCount(6);
    }
    for (const { page, session } of tabs) {
      expect(await page.evaluate(() => document.visibilityState)).toBe("visible");
      expect(
        await page.evaluate(
          () =>
            (performance.getEntriesByType("navigation")[0] as PerformanceNavigationTiming)
              .nextHopProtocol,
        ),
      ).toBe("http/1.1");
      const result = await page.evaluate(
        async ({ url, session }) => {
          const response = await fetch(url, {
            signal: AbortSignal.timeout(5000),
            headers: { "X-Docbank-Web-Session": session },
          });
          return { status: response.status, albums: await response.json() };
        },
        { url: getListPhotoAlbumsUrl(), session },
      );
      expect(result.status).toBe(200);
      expect(result.albums).toEqual([
        expect.objectContaining({ name: "Paris walks", included_count: 3, member_count: 3 }),
      ]);
    }
    await expect.poll(() => streams.size).toBe(1);
    console.log(
      "Six distinct sessions: six visible HTTP/1.1 tabs, six authenticated 200 responses, one active event stream",
    );
  } finally {
    await Promise.all(tabs.map(({ page }) => page.close()));
    await fixture.dispose();
  }
});
