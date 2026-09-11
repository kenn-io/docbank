import { execFile } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

import { expect, test } from "@playwright/test";

const execFileAsync = promisify(execFile);
const repository = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../..",
);
const binary = path.join(repository, "docbank");
const screenshots = process.env.DOCBANK_VERIFIED_INSPECTOR_SCREENSHOT_DIR;
const db17BrowserURL = process.env.DOCBANK_DB17_BROWSER_URL;
test.skip(
  !screenshots,
  "DOCBANK_VERIFIED_INSPECTOR_SCREENSHOT_DIR is required for PR-only inspector captures",
);

type Query = {
  v: 1;
  text: string;
  syntax: "simple";
  mode: "lexical";
  filters: Record<string, never>;
  sort: { field: "path"; direction: "asc" };
};

type SnapshotPage = {
  rows: {
    node_id: number;
    content_version_id: string;
    blob_hash: string;
    size: number;
    path: string;
  }[];
};

const query: Query = {
  v: 1,
  text: "",
  syntax: "simple",
  mode: "lexical",
  filters: {},
  sort: { field: "path", direction: "asc" },
};

function queryURL(raw: string): string {
  const url = new URL(raw);
  const fragment = new URLSearchParams(url.hash.slice(1));
  fragment.set("query", JSON.stringify(query));
  url.hash = fragment.toString();
  return url.toString();
}

function sessionAPI(rawURL: string) {
  const url = new URL(rawURL);
  const session = new URLSearchParams(url.hash.slice(1)).get("web_session");
  if (!session) throw new Error("Synthetic browser URL omitted its session");
  return async <T>(route: string, init: RequestInit = {}): Promise<T> => {
    const response = await fetch(`http://127.0.0.1:${url.port}${route}`, {
      ...init,
      signal: AbortSignal.timeout(60_000),
      headers: {
        Host: url.host,
        "X-Docbank-Web-Session": session,
        ...init.headers,
      },
    });
    if (!response.ok)
      throw new Error(
        `Synthetic browser API ${route} failed with ${response.status}`,
      );
    return response.json() as Promise<T>;
  };
}

test("verified inspector pins replaced content and cancels a changed source", async ({
  page,
}) => {
  test.setTimeout(300_000);
  const workspace = await mkdtemp(
    path.join(tmpdir(), "docbank-verified-inspector-"),
  );
  const vault = path.join(workspace, "vault");
  const source = path.join(workspace, "Verified inspector");
  const firstPath = path.join(source, "01-selected.txt");
  const secondPath = path.join(source, "02-second.txt");
  let running = false;
  const run = async (...args: string[]) =>
    (
      await execFileAsync(binary, args, {
        cwd: repository,
        env: { ...process.env, DOCBANK_HOME: vault },
        maxBuffer: 4 * 1024 * 1024,
        timeout: 120_000,
      })
    ).stdout.trim();

  try {
    page.setDefaultTimeout(30_000);
    await mkdir(source, { recursive: true, mode: 0o700 });
    await writeFile(
      firstPath,
      "Pinned first edition from the frozen query.\n",
      { mode: 0o600 },
    );
    await writeFile(secondPath, "Second synthetic document.\n", {
      mode: 0o600,
    });
    await run("add", source, "--dest", "/");
    await mkdir(screenshots!, { recursive: true, mode: 0o700 });

    const rawURL = await run("web", "--no-browser");
    running = true;
    const browserURL = queryURL(rawURL);
    const api = sessionAPI(browserURL);
    const frozen = await api<SnapshotPage>("/api/v1/workspace/queries", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query, page_size: 100, facets: [] }),
    });
    const selectedAuthority = frozen.rows.find((row) =>
      row.path.endsWith("/01-selected.txt"),
    );
    if (!selectedAuthority) throw new Error("Synthetic frozen row is missing");

    let delayNextBody = true;
    let releaseBody!: () => void;
    const heldBody = new Promise<void>((resolve) => {
      releaseBody = resolve;
    });
    await page.route(
      "**/api/daemon/web-download/file?ticket=*",
      async (route) => {
        if (delayNextBody) {
          delayNextBody = false;
          await heldBody;
        }
        await route.continue();
      },
    );
    const previewAuthorities: Record<string, unknown>[] = [];
    page.on("request", (request) => {
      if (
        request.method() !== "POST" ||
        !request.url().endsWith("/api/daemon/web-download")
      )
        return;
      previewAuthorities.push(
        request.postDataJSON() as Record<string, unknown>,
      );
    });

    await page.addInitScript(() =>
      localStorage.setItem("docbank-theme", "dark"),
    );
    await page.goto(browserURL);
    const editor = page.getByRole("region", { name: "Query editor" });
    await editor
      .getByRole("button", { name: "Run query", exact: true })
      .click();
    const selectedCell = page.getByRole("cell", {
      name: "/Verified inspector/01-selected.txt",
      exact: true,
    });
    const secondCell = page.getByRole("cell", {
      name: "/Verified inspector/02-second.txt",
      exact: true,
    });
    await expect(selectedCell).toBeVisible();
    await page
      .getByRole("button", { name: "Close query editor", exact: true })
      .click();

    await selectedCell.click();
    await expect(page.getByText("Verifying", { exact: false })).toBeVisible();
    const cancelled = page.waitForRequest(
      (request) =>
        request.method() === "DELETE" &&
        request.url().includes("/api/daemon/web-download?ticket="),
    );
    await secondCell.click();
    await cancelled;
    releaseBody();
    await expect(page.getByText("Second synthetic document.")).toBeVisible();

    await writeFile(
      firstPath,
      "New live head that must not replace the frozen source.\n",
      { mode: 0o600 },
    );
    await run(
      "put",
      firstPath,
      "/Verified inspector/01-selected.txt",
      "--mime-type",
      "text/plain",
    );
    await selectedCell.click();

    await expect(
      page.getByText("Pinned first edition from the frozen query."),
    ).toBeVisible();
    await expect(page.getByText("Original snapshot facts")).toBeVisible();
    await expect(page.getByText("Current live observations")).toBeVisible();
    await expect(page.getByText("SHA-256 verified")).toBeVisible();
    expect(
      page.getByText("New live head that must not replace the frozen source."),
    ).toHaveCount(0);
    expect(previewAuthorities.at(-1)).toMatchObject({
      node_id: selectedAuthority.node_id,
      version_id: selectedAuthority.content_version_id,
      blob_hash: selectedAuthority.blob_hash,
      size: selectedAuthority.size,
      purpose: "preview",
    });

    await page.screenshot({
      path: path.join(screenshots!, "web-verified-inspector.png"),
      animations: "disabled",
    });
  } finally {
    if (running) await run("daemon", "stop").catch(() => undefined);
    await rm(workspace, { recursive: true, force: true });
  }
});

test("DB17 real PDF text and fifty-step snapshot navigation", async ({ page }) => {
  test.skip(!db17BrowserURL, "DOCBANK_DB17_BROWSER_URL is supplied by the real-PDF Go fixture");
  test.setTimeout(300_000);
  page.setDefaultTimeout(30_000);
  await mkdir(screenshots!, { recursive: true, mode: 0o700 });
  const requestCounts = { resolve: 0, content: 0, pages: 0, highlights: 0 };
  page.on("request", (request) => {
    const pathname = new URL(request.url()).pathname;
    if (request.method() === "POST" && pathname === "/api/v1/renditions/text") requestCounts.resolve++;
    if (request.method() === "GET" && pathname === "/api/v1/renditions/text/content") requestCounts.content++;
    if (request.method() === "POST" && pathname === "/api/v1/queries/highlights") requestCounts.highlights++;
    if (request.method() === "POST" && /^\/api\/v1\/workspace\/queries\/[^/]+\/pages$/.test(pathname)) requestCounts.pages++;
  });
  await page.addInitScript(() => localStorage.setItem("docbank-theme", "dark"));
  await page.goto(queryURL(db17BrowserURL!));
  const editor = page.getByRole("region", { name: "Query editor" });
  await editor.getByLabel("Query expression").fill("Extracted");
  await editor.getByLabel("Processing profile").fill("archive");
  await editor.getByLabel("Documents per page").selectOption("50");
  await editor.getByRole("button", { name: "Run query", exact: true }).click();
  await expect(page.getByRole("cell", { name: "/01-evidence.pdf", exact: true })).toBeVisible();
  await editor.getByRole("button", { name: "Close query editor", exact: true }).click();
  await page.getByRole("cell", { name: "/01-evidence.pdf", exact: true }).click();
  await page.getByRole("tab", { name: "Text" }).click();
  await expect(page.getByLabel("Verified text of 01-evidence.pdf"))
    .toContainText("Extracted alpha evidence from a real synthetic PDF");
  await expect(page.getByText("Document 1 of 51", { exact: true })).toBeVisible();

  for (let position = 2; position <= 51; position++) {
    const content = page.waitForResponse((response) =>
      new URL(response.url()).pathname === "/api/v1/renditions/text/content" && response.status() === 200,
    );
    await page.getByRole("button", { name: "Next", exact: true }).click();
    await content;
    await expect(page.getByText(`Document ${position} of 51`, { exact: true })).toBeVisible();
  }
  expect(requestCounts).toEqual({ resolve: 51, content: 51, pages: 1, highlights: 1 });
  await expect(page.getByRole("button", { name: "Next", exact: true })).toBeDisabled();
  await page.getByLabel("Find in verified text").fill("PDF");
  await expect(page.getByText("1 of 2", { exact: true })).toBeVisible();
  await page.screenshot({ path: path.join(screenshots!, "web-inspector-text.png"), animations: "disabled" });
});
