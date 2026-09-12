import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

const exec = promisify(execFile);
const repository = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const screenshots = process.env.DOCBANK_PAGE_VIEWER_SCREENSHOT_DIR;
test.skip(!screenshots, "DOCBANK_PAGE_VIEWER_SCREENSHOT_DIR enables PR-only page captures");

// Independent PDF authoring: explicit boxes, rotations and a red square at the
// visible crop center. The browser measures that square against physical frames.
function mixedPDF(): Uint8Array {
  const geometries = [
    { media: [0, 0, 612, 792], crop: [0, 0, 612, 792], rotation: 0, label: "Letter" },
    { media: [0, 0, 595.276, 841.89], crop: [0, 0, 595.276, 841.89], rotation: 0, label: "A4" },
    ...[90, 180, 270].map((rotation) => ({
      media: [0, 0, 612, 792],
      crop: [72, 144, 504, 720],
      rotation,
      label: `Cropped and rotated ${rotation}`,
    })),
  ];
  const objects = [
    "<< /Type /Catalog /Pages 2 0 R >>",
    "",
    "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
  ];
  const kids: number[] = [];
  for (const g of geometries) {
    const id = objects.length + 1;
    kids.push(id);
    const x = (g.crop[0]! + g.crop[2]!) / 2,
      y = (g.crop[1]! + g.crop[3]!) / 2;
    const blueX = g.crop[0]! + (g.crop[2]! - g.crop[0]!) / 4;
    const blueY = g.crop[1]! + (g.crop[3]! - g.crop[1]!) / 4;
    const stream = `q 0.95 0.97 1 rg ${g.crop[0]} ${g.crop[1]} ${g.crop[2]! - g.crop[0]!} ${g.crop[3]! - g.crop[1]!} re f 0.1 0.2 0.4 rg BT /F1 20 Tf ${g.crop[0]! + 24} ${g.crop[3]! - 48} Td (Synthetic page ${kids.length}: ${g.label}) Tj ET 1 0 0 rg ${x - 9} ${y - 9} 18 18 re f 0 0 1 rg ${blueX - 9} ${blueY - 9} 18 18 re f Q\n`;
    objects.push(
      `<< /Type /Page /Parent 2 0 R /MediaBox [${g.media.join(" ")}] /CropBox [${g.crop.join(" ")}] /Rotate ${g.rotation} /Resources << /Font << /F1 3 0 R >> >> /Contents ${id + 1} 0 R >>`,
      `<< /Length ${Buffer.byteLength(stream)} >>\nstream\n${stream}endstream`,
    );
  }
  objects[1] = `<< /Type /Pages /Count ${kids.length} /Kids [${kids.map((id) => `${id} 0 R`).join(" ")}] >>`;
  let pdf = "%PDF-1.7\n";
  const offsets: number[] = [];
  objects.forEach((o, i) => {
    offsets.push(Buffer.byteLength(pdf));
    pdf += `${i + 1} 0 obj\n${o}\nendobj\n`;
  });
  const start = Buffer.byteLength(pdf);
  pdf += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n${offsets.map((n) => `${String(n).padStart(10, "0")} 00000 n \n`).join("")}trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${start}\n%%EOF\n`;
  return Buffer.from(pdf);
}

test("DB19 real pages retain physical alignment across zoom and DPR", async ({ browser }) => {
  test.setTimeout(300000);
  const scratch = await mkdtemp(path.join(tmpdir(), "docbank-page-browser-"));
  const vault = path.join(scratch, "vault"),
    inspector = path.join(scratch, "page-inspect");
  const run = async (...args: string[]) =>
    (
      await exec(path.join(repository, "docbank"), args, {
        cwd: repository,
        env: { ...process.env, DOCBANK_HOME: vault },
        timeout: 120000,
        maxBuffer: 4 * 1024 * 1024,
      })
    ).stdout.trim();
  let running = false;
  try {
    await mkdir(vault, { mode: 0o700 });
    await exec(
      "go",
      ["build", "-tags", "fts5", "-o", inspector, "./document/pagerender/cmd/docbank-page-inspect"],
      { cwd: repository, timeout: 120000 },
    );
    const pin = async (program: string) => {
      const absolute = program.startsWith("/")
        ? program
        : (await exec("which", [program])).stdout.trim();
      const resolved = await realpath(absolute);
      return `path = ${JSON.stringify(resolved)}\nsha256 = "${createHash("sha256")
        .update(await readFile(resolved))
        .digest("hex")}"\n`;
    };
    await writeFile(
      path.join(vault, "config.toml"),
      `[page_runtime]\ndeployment_identity = "synthetic-browser-test"\n[page_runtime.inspector]\n${await pin(inspector)}[page_runtime.renderer]\n${await pin("pdftoppm")}[page_runtime.limiter]\n${await pin("prlimit")}`,
      { mode: 0o600 },
    );
    const source = path.join(scratch, "mixed-pages.pdf");
    await writeFile(source, mixedPDF());
    running = true;
    await run("add", source, "--dest", "/");
    await mkdir(screenshots!, { recursive: true, mode: 0o700 });
    for (const dpr of [1, 2]) {
      const context = await browser.newContext({
        viewport: { width: 1440, height: 1100 },
        deviceScaleFactor: dpr,
        colorScheme: "dark",
      });
      try {
        const page = await context.newPage();
        page.setDefaultTimeout(30000);
        const requests: string[] = [];
        page.on("request", (r) => {
          if (r.url().includes("/pages/")) requests.push(r.url());
        });
        await page.goto(await run("web", "--no-browser"));
        await page.getByRole("cell", { name: "mixed-pages.pdf", exact: true }).click();
        const viewer = page.getByRole("region", { name: "Pages of mixed-pages.pdf" });
        if (dpr === 1) {
          await expect(viewer.getByText(/No page inventory/)).toBeVisible();
          expect(requests.filter((v) => v.endsWith("/jobs"))).toHaveLength(0);
          await viewer.getByRole("button", { name: "Render page 1", exact: true }).click();
        }
        const expected = [
          [85000, 110000],
          [82677, 116929],
          [80000, 60000],
          [60000, 80000],
          [80000, 60000],
        ];
        const bluePositions = [
          [0.25, 0.75],
          [0.25, 0.75],
          [0.25, 0.25],
          [0.75, 0.25],
          [0.75, 0.75],
        ];
        for (let index = 0; index < expected.length; index++) {
          if (index > 0) {
            await viewer.getByRole("button", { name: "Next page", exact: true }).click();
            if (dpr === 1)
              await viewer
                .getByRole("button", { name: `Render page ${index + 1}`, exact: true })
                .click();
          }
          const image = viewer.getByRole("img", {
            name: `Verified page ${index + 1} of mixed-pages.pdf`,
            exact: true,
          });
          await expect(image).toBeVisible();
          for (const zoom of ["Fit width", "50%", "100%", "150%"]) {
            await viewer.getByRole("combobox", { name: /^Zoom:/ }).click();
            await page.getByRole("option", { name: zoom, exact: true }).click();
            const geometry = await image.evaluate((element) => {
              const img = element as HTMLImageElement,
                rect = img.getBoundingClientRect();
              const canvas = document.createElement("canvas");
              canvas.width = img.naturalWidth;
              canvas.height = img.naturalHeight;
              const ctx = canvas.getContext("2d")!;
              ctx.drawImage(img, 0, 0);
              const pixels = ctx.getImageData(0, 0, canvas.width, canvas.height).data;
              let minX = canvas.width,
                minY = canvas.height,
                maxX = -1,
                maxY = -1;
              let blueMinX = canvas.width,
                blueMinY = canvas.height,
                blueMaxX = -1,
                blueMaxY = -1;
              for (let y = 0; y < canvas.height; y++)
                for (let x = 0; x < canvas.width; x++) {
                  const i = (y * canvas.width + x) * 4;
                  if (pixels[i]! > 240 && pixels[i + 1]! < 20 && pixels[i + 2]! < 20) {
                    minX = Math.min(minX, x);
                    minY = Math.min(minY, y);
                    maxX = Math.max(maxX, x);
                    maxY = Math.max(maxY, y);
                  }
                  if (pixels[i]! < 20 && pixels[i + 1]! < 20 && pixels[i + 2]! > 240) {
                    blueMinX = Math.min(blueMinX, x);
                    blueMinY = Math.min(blueMinY, y);
                    blueMaxX = Math.max(blueMaxX, x);
                    blueMaxY = Math.max(blueMaxY, y);
                  }
                }
              return {
                width: rect.width,
                height: rect.height,
                centerX: (minX + maxX + 1) / 2 / canvas.width,
                centerY: (minY + maxY + 1) / 2 / canvas.height,
                blueX: (blueMinX + blueMaxX + 1) / 2 / canvas.width,
                blueY: (blueMinY + blueMaxY + 1) / 2 / canvas.height,
                padding: getComputedStyle(img.parentElement!).padding,
                border: getComputedStyle(img.parentElement!).borderWidth,
                dpr: devicePixelRatio,
              };
            });
            expect(geometry.width / geometry.height).toBeCloseTo(
              expected[index]![0]! / expected[index]![1]!,
              3,
            );
            expect(geometry.centerX).toBeCloseTo(0.5, 3);
            expect(geometry.centerY).toBeCloseTo(0.5, 3);
            expect(geometry.blueX).toBeCloseTo(bluePositions[index]![0]!, 3);
            expect(geometry.blueY).toBeCloseTo(bluePositions[index]![1]!, 3);
            expect(geometry.padding).toBe("0px");
            expect(geometry.border).toBe("0px");
            expect(geometry.dpr).toBe(dpr);
            if (zoom !== "Fit width")
              expect(geometry.width).toBeCloseTo(
                ((expected[index]![0]! / 10000) * 96 * parseInt(zoom)) / 100,
                1,
              );
          }
          if (dpr === 1 && index === 2) {
            await viewer.getByRole("combobox", { name: /^Zoom:/ }).click();
            await page.getByRole("option", { name: "Fit width", exact: true }).click();
            await viewer.scrollIntoViewIfNeeded();
            await page.screenshot({
              path: path.join(screenshots!, "web-page-viewer.png"),
              animations: "disabled",
            });
          }
        }
        await expect(viewer.getByRole("button", { name: "Next page", exact: true })).toBeDisabled();
        expect(requests.filter((v) => v.endsWith("/jobs"))).toHaveLength(dpr === 1 ? 5 : 0);
      } finally {
        await context.close();
      }
    }
  } finally {
    if (running) await run("daemon", "stop");
    await rm(scratch, { recursive: true, force: true });
  }
});
