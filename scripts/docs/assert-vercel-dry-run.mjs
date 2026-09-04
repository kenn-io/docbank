import { readFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";


const requiredUploads = [
  "vercel.json",
  "website/index.html",
  "docs/zensical.toml",
  "docs/uv.lock",
  "scripts/vercel-install-docs.sh",
  "scripts/vercel-build-docs.sh",
  "scripts/sync-docs-assets.sh",
  "scripts/docs-assets.ref",
  "scripts/docs-assets.txt",
  "scripts/docs/build.mjs",
  "scripts/docs/verify-site.mjs",
  "scripts/install.sh",
  "scripts/install.ps1",
];

const maximumUploadBytes = 10 * 1024 * 1024;

const allowedUploads = new Set([
  ...requiredUploads,
  "LICENSES/Inter-OFL-1.1.txt",
  "LICENSES/JetBrains-Mono-OFL-1.1.txt",
  "docs/llms.txt",
  "docs/overrides/main.html",
  "docs/pyproject.toml",
  "docs/scripts/check_built_site.py",
  "docs/scripts/check_markdown_sources.py",
  "docs/stylesheets/extra.css",
  "docs/zensical-docs.sh",
  "website/favicon.svg",
  "website/fonts/Inter-Medium.woff2",
  "website/fonts/Inter-Regular.woff2",
  "website/fonts/Inter-SemiBold.woff2",
  "website/fonts/JetBrainsMono-Bold.woff2",
  "website/fonts/JetBrainsMono-Regular.woff2",
  "website/fonts/JetBrainsMono-SemiBold.woff2",
  "website/guide.md",
  "website/guide/index.html",
  "website/index.html",
  "website/index.md",
  "website/scripts/site.js",
  "website/styles/site.css",
]);

const allowedUploadPatterns = [
  /^docs\/(?!README\.md$)[^/]+\.md$/,
  /^docs\/(?:agents|architecture|usage)\/[^/]+\.md$/,
  /^website\/assets\/[a-z0-9][a-z0-9-]*\.svg$/,
];


function normalizeEntry(entry) {
  const reported = typeof entry === "string" ? entry : entry?.path;
  if (typeof reported !== "string" || reported.length === 0) {
    throw new Error("Vercel dry-run entry is missing a path field");
  }
  let normalized = reported.replaceAll("\\", "/");
  while (normalized.startsWith("./")) normalized = normalized.slice(2);
  if (normalized.startsWith("/") || normalized.split("/").includes("..")) {
    throw new Error(`Vercel dry-run entry is not repository-relative: ${reported}`);
  }
  return normalized;
}


function uploadSize(entry) {
  if (typeof entry === "string") return 0;
  if (!Number.isSafeInteger(entry?.size) || entry.size < 0) {
    throw new Error(`Vercel dry-run entry has an invalid size: ${entry?.path ?? "<unknown>"}`);
  }
  return entry.size;
}


function isAllowed(upload) {
  return allowedUploads.has(upload)
    || allowedUploadPatterns.some((pattern) => pattern.test(upload));
}


export function assertUploadBoundary(entries) {
  if (!Array.isArray(entries)) throw new Error("Vercel dry-run report must be a JSON array");
  const normalized = entries.map((entry) => ({
    path: normalizeEntry(entry),
    size: uploadSize(entry),
  }));
  const uploads = new Set(normalized.map((entry) => entry.path));
  for (const required of requiredUploads) {
    if (!uploads.has(required)) throw new Error(`missing required upload: ${required}`);
  }
  for (const upload of uploads) {
    if (!isAllowed(upload)) throw new Error(`forbidden upload: ${upload}`);
  }
  const totalBytes = normalized.reduce((total, entry) => total + entry.size, 0);
  if (totalBytes > maximumUploadBytes) {
    throw new Error(`Vercel upload exceeds 10 MiB limit: ${totalBytes} bytes`);
  }
  return [...uploads].sort();
}


export async function readDryRunReport(reportPath) {
  let report;
  try {
    report = JSON.parse(await readFile(reportPath, "utf8"));
  } catch (error) {
    throw new Error(`invalid Vercel dry-run JSON: ${error.message}`, { cause: error });
  }
  const entries = Array.isArray(report) ? report : report?.files;
  if (!Array.isArray(entries)) {
    throw new Error("Vercel dry-run report must be a JSON array or contain a files array");
  }
  entries.forEach((entry) => {
    normalizeEntry(entry);
    uploadSize(entry);
  });
  return entries;
}


const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  const reportPath = process.argv[2];
  if (!reportPath || process.argv.length !== 3) {
    process.stderr.write("usage: node scripts/docs/assert-vercel-dry-run.mjs <report.json>\n");
    process.exitCode = 2;
  } else {
    try {
      const uploads = assertUploadBoundary(await readDryRunReport(reportPath));
      process.stdout.write(`validated ${uploads.length} Vercel upload files\n`);
    } catch (error) {
      process.stderr.write(`${error.message}\n`);
      process.exitCode = 1;
    }
  }
}
