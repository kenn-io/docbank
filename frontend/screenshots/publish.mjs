import {
  access,
  open,
  readdir,
  rename as renamePath,
  rm,
} from "node:fs/promises";
import path from "node:path";


const pngSignature = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
const screenshotName = /^[A-Za-z0-9][A-Za-z0-9._-]*\.png$/;


async function exists(target) {
  try {
    await access(target);
    return true;
  } catch (error) {
    if (error?.code === "ENOENT") return false;
    throw error;
  }
}


function validateNames(names) {
  if (!Array.isArray(names) || names.length === 0) {
    throw new Error("screenshot manifest is empty");
  }
  const sorted = [...names].sort();
  if (names.some((name, index) => name !== sorted[index])) {
    throw new Error("screenshot manifest is not sorted");
  }
  if (new Set(names).size !== names.length) {
    throw new Error("screenshot manifest contains duplicate names");
  }
  for (const name of names) {
    if (!screenshotName.test(name) || path.basename(name) !== name) {
      throw new Error(`invalid screenshot name: ${name}`);
    }
  }
}


async function validateStaging(staging, names) {
  const entries = await readdir(staging, { withFileTypes: true });
  const files = entries.filter((entry) => entry.isFile()).map((entry) => entry.name).sort();
  const unexpectedEntry = entries.find((entry) => !entry.isFile());
  if (unexpectedEntry) {
    throw new Error(`unexpected screenshot entry: ${unexpectedEntry.name}`);
  }
  for (const name of names) {
    if (!files.includes(name)) throw new Error(`missing screenshot: ${name}`);
  }
  for (const name of files) {
    if (!names.includes(name)) throw new Error(`unexpected screenshot: ${name}`);
  }
  for (const name of names) {
    const file = await open(path.join(staging, name), "r");
    try {
      const header = Buffer.alloc(pngSignature.length);
      const { bytesRead } = await file.read(header, 0, header.length, 0);
      if (bytesRead !== pngSignature.length || !header.equals(pngSignature)) {
        throw new Error(`invalid PNG signature: ${name}`);
      }
    } finally {
      await file.close();
    }
  }
}


export async function publishScreenshots({
  output,
  staging,
  names,
  rename = async (from, to, realRename) => realRename(from, to),
}) {
  validateNames(names);
  if (path.dirname(output) !== path.dirname(staging)) {
    throw new Error("screenshot staging directory must be beside output");
  }
  await validateStaging(staging, names);

  const previous = path.join(
    path.dirname(output),
    `.${path.basename(output)}.previous`,
  );
  await rm(previous, { recursive: true, force: true });
  const hadOutput = await exists(output);
  if (hadOutput) await rename(output, previous, renamePath);
  try {
    await rename(staging, output, renamePath);
  } catch (error) {
    if (hadOutput) await rename(previous, output, renamePath);
    throw error;
  }
  await rm(previous, { recursive: true, force: true });
}
