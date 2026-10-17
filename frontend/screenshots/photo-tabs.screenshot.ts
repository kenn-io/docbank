import { test, expect, type Page, type Request } from "@playwright/test";
import { createServer } from "node:net";
import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import path from "node:path";
import { getListPhotoAlbumsUrl } from "../src/generated/docbank.js";
import { photoFixture, repository } from "./photo-fixture.js";

test.use({ launchOptions: { args: ["--host-resolver-rules=MAP archive.example.test 127.0.0.1"] } });

test.skip(
  !process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR,
  "DOCBANK_PHOTOS_SCREENSHOT_DIR enables synthetic photo proof",
);

async function sixVisiblePhotoTabs(
  context: import("@playwright/test").BrowserContext,
  insecureHTTP: boolean,
) {
  test.setTimeout(180_000);
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "six-tabs-proof-"));
  const fixture = photoFixture(workspace);
  const tabs: { page: Page; session: string }[] = [];
  const streams = new Set<Request>();
  let holder: Page | undefined;
  let insecureOrigin = "";
  const apiKey = "synthetic-api-key-0123456789abcdef";
  try {
    await fixture.seed(6);
    if (insecureHTTP) {
      const port = await new Promise<number>((resolve, reject) => {
        const listener = createServer();
        listener.once("error", reject);
        listener.listen(0, "127.0.0.1", () => {
          const address = listener.address();
          if (!address || typeof address === "string") {
            reject(new Error("missing synthetic listener address"));
            return;
          }
          listener.close((error) => (error ? reject(error) : resolve(address.port)));
        });
      });
      insecureOrigin = `http://archive.example.test:${port}`;
      await writeFile(
        path.join(workspace, "vault", "config.toml"),
        `[server]\napi_port = ${port}\napi_key = "${apiKey}"\nidle_timeout = "0"\n[web]\npublic_origin = "${insecureOrigin}"\ntrust_private_network = true\nsession_lifetime = "24h"\n`,
        { mode: 0o600 },
      );
      await fixture.run("web", "--no-browser");
    }
    for (let index = 0; index < 6; index++) {
      const url = insecureHTTP
        ? new URL(insecureOrigin)
        : new URL(await fixture.run("web", "--no-browser"));
      let session = new URLSearchParams(url.hash.slice(1)).get("web_session") ?? "";
      const page = await context.newPage();
      const tab = { page, session };
      tabs.push(tab);
      page.on("response", (response) => {
        if (
          new URL(response.url()).pathname === "/api/v1/photos/events" &&
          response.status() === 200
        ) {
          holder = page;
          streams.add(response.request());
          console.log("Active photo event streams:", streams.size);
        }
      });
      page.on("requestfinished", (request) => streams.delete(request));
      page.on("requestfailed", (request) => streams.delete(request));
      if (insecureHTTP) {
        await page.goto(url.href, { waitUntil: "domcontentloaded", timeout: 20_000 });
        const login = page.waitForResponse((response) =>
          response.url().endsWith("/api/daemon/web-auth/login"),
        );
        await page.getByLabel("API key").fill(apiKey);
        await page.getByRole("button", { name: "Sign in", exact: true }).click();
        const response = await login;
        expect(response.ok()).toBe(true);
        const value = (await response.json()) as { session?: { token?: string } };
        session = value.session?.token ?? "";
        expect(session).toBeTruthy();
        await expect(page.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
        await page.getByRole("button", { name: "Photos", exact: true }).click();
        tab.session = session;
      } else {
        expect(session).toBeTruthy();
        url.pathname = "/photos";
        await page.goto(url.href, { waitUntil: "domcontentloaded", timeout: 20_000 });
      }
      expect(
        tabs
          .filter((candidate) => candidate !== tab)
          .some((candidate) => candidate.session === session),
      ).toBe(false);
      await expect(page.locator("[data-asset]"), `Photo list in tab ${index + 1}`).toHaveCount(6);
    }
    for (const { page, session } of tabs) {
      expect(await page.evaluate(() => document.visibilityState)).toBe("visible");
      const originSupport = await page.evaluate(() => ({
        secureContext: isSecureContext,
        locks: typeof navigator.locks,
        broadcastChannel: typeof BroadcastChannel,
      }));
      if (insecureHTTP) {
        expect(originSupport).toEqual({
          secureContext: false,
          locks: "undefined",
          broadcastChannel: "function",
        });
      }
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
    await expect.poll(() => streams.size).toBe(insecureHTTP ? 0 : 1);
    const first = await tabs[0]!.page.locator("[data-asset]").first().getAttribute("data-asset");
    if (insecureHTTP) {
      await fixture.run("photos", "assets", "exclude", first!);
    } else {
      const lockedHolder = holder!;
      // Keep this tab's access checks locked while the follower acquires a shared cookie.
      await lockedHolder.route("**/api/v1/photos/hidden", async (route) => {
        const headers = route.request().headers();
        headers.cookie = "";
        const response = await fetch(route.request().url(), { headers });
        const body = await response.text();
        expect(JSON.parse(body).expires_at).toBeUndefined();
        await route.fulfill({ status: response.status, contentType: "application/json", body });
      });
      const follower = tabs.find(({ page }) => page !== holder)!;
      await follower.page.getByRole("button", { name: "Hidden", exact: true }).click();
      await follower.page.getByLabel("Passcode", { exact: true }).fill("synthetic-passcode");
      await follower.page.getByRole("button", { name: "Set passcode", exact: true }).click();
      await expect(follower.page.getByText(/Locks in/)).toBeVisible();
      await follower.page.getByRole("button", { name: "Library", exact: true }).click();
      await follower.page
        .locator(`[data-asset="${first}"]`)
        .getByRole("button", { name: /^Select / })
        .click({ button: "right" });
      await follower.page.getByRole("menuitem", { name: "Hide", exact: true }).click();
      await follower.page.getByRole("button", { name: "Hidden", exact: true }).click();
      await expect(follower.page.locator(`[data-asset="${first}"]`)).toBeVisible();
      expect(holder).toBe(lockedHolder);
      expect(streams.size).toBe(1);
      const status = await follower.page.evaluate(
        async ({ id, session }) => {
          const headers = { "X-Docbank-Web-Session": session };
          const page = (await (
            await fetch("/api/v1/photos/assets/query", {
              method: "POST",
              headers: { ...headers, "Content-Type": "application/json" },
              body: JSON.stringify({
                query: { syntax: "simple", text: "", filters: { asset_ids: [id] } },
                page_size: 1,
                hidden: true,
              }),
            })
          ).json()) as { items: { revision: number }[] };
          const asset = page.items[0]!;
          return (
            await fetch(`/api/v1/photos/assets/${id}/trash`, {
              method: "POST",
              headers: { ...headers, "If-Match": String(asset.revision) },
            })
          ).status;
        },
        { id: first!, session: follower.session },
      );
      expect(status).toBe(200);
      await expect(follower.page.locator(`[data-asset="${first}"]`)).toHaveCount(0);
      await follower.page.getByRole("button", { name: "Library", exact: true }).click();
    }
    for (const { page } of tabs) {
      await expect(page.locator(`[data-asset="${first}"]`)).toHaveCount(0);
      await expect(page.locator("[data-asset]")).toHaveCount(5);
    }
    if (!insecureHTTP) expect(holder).toBeTruthy();
    const departed = holder ?? tabs[0]!.page;
    await departed.close();
    const remaining = tabs.filter(({ page }) => page !== departed);
    await expect.poll(() => streams.size).toBe(insecureHTTP ? 0 : 1);
    if (!insecureHTTP) await expect.poll(() => holder !== departed).toBe(true);
    const second = await remaining[0]!.page
      .locator("[data-asset]")
      .first()
      .getAttribute("data-asset");
    await fixture.run("photos", "assets", "exclude", second!);
    for (const { page } of remaining) {
      await expect(page.locator(`[data-asset="${second}"]`)).toHaveCount(0);
      await expect(page.locator("[data-asset]")).toHaveCount(4);
    }
    if (insecureHTTP) {
      const output = process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR!;
      await mkdir(output, { recursive: true, mode: 0o700 });
      await remaining[0]!.page.screenshot({
        path: path.join(output, "photo-tabs-http11.png"),
        animations: "disabled",
      });
    }
    console.log(
      `Six distinct sessions: six visible ${insecureHTTP ? "insecure HTTP/1.1" : "HTTP/1.1"} tabs, six authenticated 200 responses, ${insecureHTTP ? "no event streams" : "one active event stream"}`,
    );
  } finally {
    await Promise.all(tabs.map(({ page }) => page.close()));
    await fixture.dispose();
  }
}

test("six visible tabs retain HTTP/1.1 request capacity", async ({ context }) => {
  await sixVisiblePhotoTabs(context, false);
});

test("six visible insecure HTTP tabs refresh without event streams", async ({ context }) => {
  await sixVisiblePhotoTabs(context, true);
});
