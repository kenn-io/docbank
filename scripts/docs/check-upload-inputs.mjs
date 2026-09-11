import { lstat, readFile, readdir } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import { assertUploadBoundary } from "./assert-vercel-dry-run.mjs";

const require = createRequire(new URL("../../frontend/package.json", import.meta.url));
const ignore = require("ignore");

export async function collectUploadInputs(root) {
  const excluded = ignore().add(await readFile(path.join(root, ".vercelignore"), "utf8"));
  const files = [];
  async function visit(directory) {
    const entries = await readdir(path.join(root, directory), { withFileTypes: true });
    for (const entry of entries) {
      const relative = directory ? `${directory}/${entry.name}` : entry.name;
      if (excluded.ignores(relative + (entry.isDirectory() ? "/" : ""))) continue;
      if (entry.isDirectory()) {
        await visit(relative);
      } else if (entry.isFile()) {
        const { size } = await lstat(path.join(root, relative));
        files.push({ path: relative, size });
      } else {
        throw new Error(`unsupported upload input: ${relative}`);
      }
    }
  }
  await visit("");
  return files.sort((a, b) => a.path.localeCompare(b.path));
}

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  try {
    const root = fileURLToPath(new URL("../../", import.meta.url));
    const files = await collectUploadInputs(root);
    const uploads = assertUploadBoundary(files);
    process.stdout.write(`validated ${uploads.length} local documentation upload files\n`);
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
