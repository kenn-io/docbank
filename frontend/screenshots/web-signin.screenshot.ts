import { expect, test } from "@playwright/test";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";

const run = promisify(execFile);
const repository = path.resolve(import.meta.dirname, "../..");
const binary = path.join(repository, "bin", process.platform === "win32" ? "docbank.exe" : "docbank");

test.use({ launchOptions: { args: ["--host-resolver-rules=MAP archive.example.test 127.0.0.1"] } });

function envFor(vault: string): NodeJS.ProcessEnv {
 const env: NodeJS.ProcessEnv = { DOCBANK_HOME: vault, DOCBANK_TELEMETRY_ENABLED: "0" };
 for (const name of ["PATH", "HOME", "TMPDIR", "TEMP", "TMP", "SystemRoot", "USERPROFILE", "LOCALAPPDATA"]) {
  if (process.env[name]) env[name] = process.env[name];
 }
 return env;
}

const cli = async (vault: string, ...args: string[]): Promise<string> =>
 (await run(binary, args, { cwd: repository, env: envFor(vault), timeout: 60_000 })).stdout.trim();

async function freePort(): Promise<number> {
 const reserve = createServer();
 await new Promise<void>((resolve, reject) => {
  reserve.once("error", reject);
  reserve.listen(0, "127.0.0.1", resolve);
 });
 const address = reserve.address();
 if (!address || typeof address === "string") throw new Error("missing synthetic listener address");
 await new Promise<void>((resolve, reject) => reserve.close((error) => error ? reject(error) : resolve()));
 return address.port;
}

async function prepareVault(vault: string, port: number, apiKey: string): Promise<string> {
 await mkdir(vault, { recursive: true, mode: 0o700 });
 const origin = `http://archive.example.test:${port}`;
 await writeFile(path.join(vault, "config.toml"), `[server]\napi_port = ${port}\napi_key = "${apiKey}"\nidle_timeout = "0"\n[web]\npublic_origin = "${origin}"\ntrust_private_network = true\nsession_lifetime = "24h"\n`, { mode: 0o600 });
 return origin;
}

// Exercise the implemented interface against an isolated synthetic real daemon.
test("key login, insecure HTTP crypto, port isolation, independent tabs, restart and CLI handoff", async ({ page }) => {
 const workspace = await mkdtemp(path.join(tmpdir(), "docbank-web-signin-"));
 const vault = path.join(workspace, "vault");
 const otherVault = path.join(workspace, "other-vault");
 const apiKey = "synthetic-api-key-0123456789abcdef";
 let origin = "";
 let otherOrigin = "";
 try {
  const firstPort = await freePort();
  let otherPort = await freePort();
  while (otherPort === firstPort) otherPort = await freePort();
  origin = await prepareVault(vault, firstPort, apiKey);
  otherOrigin = await prepareVault(otherVault, otherPort, apiKey);
  await cli(vault, "daemon", "start");
  await cli(otherVault, "daemon", "start");
  await cli(vault, "tag", "create", "tax");
  await page.goto(origin);
  const browserCrypto = await page.evaluate(() => ({
   secureContext: isSecureContext,
   randomUUID: typeof crypto.randomUUID,
   subtle: typeof crypto.subtle,
   getRandomValues: typeof crypto.getRandomValues,
  }));
  expect(browserCrypto).toEqual({ secureContext: false, randomUUID: "undefined", subtle: "undefined", getRandomValues: "function" });
  await expect(page.getByLabel("API key")).toBeVisible();
  const label = await page.locator('label[for="api-key"]').boundingBox();
  const field = await page.getByLabel("API key").boundingBox();
  const button = await page.getByRole("button", { name: "Sign in", exact: true }).boundingBox();
  expect(label && field && label.y + label.height <= field.y).toBeTruthy();
  expect(field && button && field.y + field.height <= button.y).toBeTruthy();
  const captures = process.env.DOCBANK_SIGNIN_SCREENSHOT_DIR;
  if (captures) {
   await mkdir(captures, { recursive: true, mode: 0o700 });
   await page.screenshot({ path: path.join(captures, "web-signin.png") });
  }
  await page.getByLabel("API key").fill("wrong");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("invalid API key");
  await expect(page.getByLabel("API key")).toHaveValue("");
  const login = async (tab: typeof page): Promise<string> => {
   const responsePromise = tab.waitForResponse(response => response.url().endsWith("/api/daemon/web-auth/login"));
   await tab.getByLabel("API key").fill(apiKey);
   await tab.getByRole("button", { name: "Sign in", exact: true }).click();
   const response = await responsePromise;
   expect(response.ok()).toBe(true);
   const value = await response.json() as { session?: { token?: string } };
   if (!value.session?.token) throw new Error("login response omitted the scoped tab token");
   await expect(tab.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
   await expect(tab.getByRole("alert")).toHaveCount(0);
   await expect(tab.getByRole("button", { name: "Upload files to current folder", exact: true })).toBeEnabled();
   return value.session.token;
  };
  const firstTagsResponsePromise = page.waitForResponse(response => new URL(response.url()).pathname === "/api/v1/tags");
  const firstToken = await login(page);
  const firstStatus = (await firstTagsResponsePromise).status();
  const firstCookies = await page.context().cookies([origin]);
  const firstCookie = firstCookies.find(value => value.name.startsWith("docbank_session_"));
  expect(firstCookie?.httpOnly).toBe(true); expect(firstCookie?.sameSite).toBe("Strict");
  expect(firstCookie?.secure).toBe(false); expect(firstCookie?.domain).toBe("archive.example.test");
  expect(firstCookie?.value).not.toBe(apiKey);

  await page.getByRole("button", { name: "Upload files to current folder", exact: true }).click();
  await page.getByLabel("Choose local files", { exact: true }).setInputFiles({
   name: "web-signin-session-expiry.txt",
   mimeType: "text/plain",
   buffer: Buffer.from("synthetic session expiry proof"),
  });
  await page.getByRole("button", { name: "Upload 1 file", exact: true }).click();
  await expect(page.getByText("Added", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Close upload documents", exact: true }).click();
  await cli(vault, "tag", "assign", "tax", "/web-signin-session-expiry.txt");

  const otherOriginTab = await page.context().newPage();
  await otherOriginTab.goto(otherOrigin);
  const otherTagsResponsePromise = otherOriginTab.waitForResponse(response => new URL(response.url()).pathname === "/api/v1/tags");
  await login(otherOriginTab);
  const otherStatus = (await otherTagsResponsePromise).status();
  const portScopedCookies = (await page.context().cookies([origin, otherOrigin]))
   .filter(value => value.name.startsWith("docbank_session_"));
  expect(portScopedCookies).toHaveLength(2);
  expect(new Set(portScopedCookies.map(value => value.name)).size).toBe(2);
  expect(new Set(portScopedCookies.map(value => value.value)).size).toBe(2);
  expect(firstStatus).toBe(200);
  expect(otherStatus).toBe(200);

  const second = await page.context().newPage();
  await second.goto(origin);
  const secondToken = await login(second);
  expect(secondToken).not.toBe(firstToken);
  const sharedOriginCookies = (await page.context().cookies([origin]))
   .filter(value => value.name.startsWith("docbank_session_"));
  expect(sharedOriginCookies.find(value => value.name === firstCookie?.name)?.value).toBe(firstCookie?.value);
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByLabel("API key")).toBeVisible();
  await expect(second.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
  const listed = JSON.parse(await cli(vault, "web", "sessions", "--json")) as { items: { id: string }[] };
  expect(listed.items).toHaveLength(1);
  await second.getByRole("cell", { name: "web-signin-session-expiry.txt", exact: true }).click();
  await second.getByRole("button", { name: "Manage", exact: true }).click();
  const manageTags = second.getByRole("dialog", { name: "Manage tags for web-signin-session-expiry.txt" });
  await expect(manageTags).toContainText("tax");
  await cli(vault, "web", "sessions", "revoke", listed.items[0]!.id);
  await manageTags.getByRole("button", { name: "Remove tag tax", exact: true }).click();
  await expect(second.getByLabel("API key")).toBeVisible();
  await expect(second.getByRole("alert")).toContainText("session expired or was rejected");
  await expect(second.getByRole("dialog", { name: "Manage tags for web-signin-session-expiry.txt" })).toHaveCount(0);
  await login(second);
  await expect(second.getByRole("dialog", { name: "Manage tags for web-signin-session-expiry.txt" })).toHaveCount(0);
  await second.close();
  await otherOriginTab.close();
  await cli(vault, "daemon", "stop");
  await cli(vault, "daemon", "start");
  await page.reload();
  await expect(page.getByLabel("API key")).toBeVisible();
  await login(page);
  // The original CLI handoff retains its separate random origin and upload proof.
  await page.goto(await cli(vault, "web", "--no-browser"));
  await expect(page.getByRole("button", { name: "Lock web session", exact: true })).toBeVisible();
 } finally {
  await cli(otherVault, "daemon", "stop").catch(() => {});
  await cli(vault, "daemon", "stop").catch(() => {});
  await rm(workspace, { recursive: true, force: true });
 }
});
