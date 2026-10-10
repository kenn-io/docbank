import { mkdir, writeFile } from "node:fs/promises";
import path from "node:path";

export async function createSyntheticCameraFiles(directory: string, count: number, startIndex = 1) {
  await mkdir(directory, { recursive: true, mode: 0o700 });
  for (let index = startIndex; index < startIndex + count; index += 1) {
    const content = index === 1 ? "synthetic-jpeg" : `synthetic-jpeg-${index}`;
    await writeFile(path.join(directory, `IMG_${String(index).padStart(4, "0")}.JPG`), content, { mode: 0o600 });
  }
}
