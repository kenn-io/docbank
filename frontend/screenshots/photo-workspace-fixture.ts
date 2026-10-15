import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";

export const output = process.env.DOCBANK_PHOTOS_SCREENSHOT_DIR;
export async function withPhotoFixture(
  count: number,
  preview: string,
  use: (run: (...args: string[]) => Promise<string>) => Promise<void>,
  rejects = false,
) {
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "photos-proof-"));
  const { run, seed, dispose } = photoFixture(workspace);
  try {
    await mkdir(output!, { recursive: true });
    await seed(count, ...(rejects ? ["--rejects"] : []));
    await use(run);
  } finally {
    if (process.env.DOCBANK_KEEP_PHOTO_PREVIEW) {
      const url = new URL(await run("web", "--no-browser"));
      url.pathname = preview === "hidden" ? "/photos/hidden" : "/photos";
      await writeFile(
        path.join(output!, `${preview}-preview.json`),
        JSON.stringify({ url: url.href, workspace }, null, 2),
      );
    } else {
      await dispose();
    }
  }
}
