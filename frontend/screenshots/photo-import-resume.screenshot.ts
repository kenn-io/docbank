import { chromium, expect, test } from "@playwright/test";
import { mkdir, mkdtemp, readFile, writeFile, stat, utimes } from "node:fs/promises";
import path from "node:path";
import type { PhotoBrowsePage } from "../src/generated/docbank.js";
import { photoFixture, repository } from "./photo-fixture.js";

test("resumes the complete selection and source after a daemon and browser restart", async () => {
  test.setTimeout(180_000);
  await mkdir(path.join(repository, ".superpowers"), { recursive: true });
  const workspace = await mkdtemp(path.join(repository, ".superpowers", "import-resume-"));
  const fixture = photoFixture(workspace);
  const profile = path.join(workspace, "browser");
  let browser = await chromium.launchPersistentContext(profile);
  try {
    const files: string[] = [];
    const canvasPage = await browser.newPage();
    for (let index = 0; index < 3; index++) {
      const jpeg = Buffer.from(
        await canvasPage.evaluate((index) => {
          const canvas = document.createElement("canvas");
          canvas.width = canvas.height = 64;
          const ctx = canvas.getContext("2d")!;
          ctx.fillStyle = `rgb(${80 + index * 40},120,160)`;
          ctx.fillRect(0, 0, 64, 64);
          return canvas.toDataURL("image/jpeg").split(",")[1]!;
        }, index),
        "base64",
      );
      // TIFF IFD0 contains one camera and a capture time one second apart.
      const values = ["Synthetic\0", "Camera\0", `2026:01:02 12:00:0${index}\0`];
      const tiff = Buffer.alloc(68 + values.reduce((n, value) => n + value.length, 0));
      tiff.write("II");
      tiff.writeUInt16LE(42, 2);
      tiff.writeUInt32LE(8, 4);
      tiff.writeUInt16LE(3, 8);
      let offset = 68;
      for (const [i, tag] of [0x010f, 0x0110].entries()) {
        const entry = 10 + i * 12;
        tiff.writeUInt16LE(tag, entry);
        tiff.writeUInt16LE(2, entry + 2);
        tiff.writeUInt32LE(values[i]!.length, entry + 4);
        tiff.writeUInt32LE(offset, entry + 8);
        tiff.write(values[i]!, offset);
        offset += values[i]!.length;
      }
      tiff.writeUInt16LE(0x8769, 34);
      tiff.writeUInt16LE(4, 36);
      tiff.writeUInt32LE(1, 38);
      tiff.writeUInt32LE(50, 42);
      tiff.writeUInt16LE(1, 50);
      tiff.writeUInt16LE(0x9003, 52);
      tiff.writeUInt16LE(2, 54);
      tiff.writeUInt32LE(values[2]!.length, 56);
      tiff.writeUInt32LE(offset, 60);
      tiff.write(values[2]!, offset);
      const segment = Buffer.concat([Buffer.from("Exif\0\0"), tiff]);
      const header = Buffer.from([0xff, 0xe1, 0, 0]);
      header.writeUInt16BE(segment.length + 2, 2);
      const file = path.join(workspace, `frame-${index}.jpg`);
      await writeFile(
        file,
        Buffer.concat([jpeg.subarray(0, 2), header, segment, jpeg.subarray(2)]),
      );
      const modified = new Date("2026-01-02T12:00:00Z");
      await utimes(file, modified, modified);
      files.push(file);
    }
    await canvasPage.close();
    const plans: string[] = [];
    const requests = new Set<string>();
    const open = async () => {
      const url = new URL(await fixture.run("web", "--no-browser"));
      url.pathname = "/photos";
      const page = await browser.newPage();
      let browse: object | undefined;
      page.on("request", (request) => {
        if (!browse && new URL(request.url()).pathname === "/api/v1/photos/assets/query")
          browse = request.postDataJSON() as object;
      });
      page.setDefaultTimeout(15_000);
      page.on("websocket", (socket) =>
        socket.on("framesent", ({ payload }) => {
          if (typeof payload !== "string" || !payload.includes('"plan_photo_import"')) return;
          // A plan resent with the hashes the daemon asked for keeps its request.
          const plan = JSON.parse(payload);
          if (!requests.has(plan.request_id)) {
            requests.add(plan.request_id);
            plans.push(plan.source_id);
          }
        }),
      );
      await page.goto(url.href);
      await page.getByRole("button", { name: "Add photos", exact: true }).first().click();
      await expect(page.getByRole("button", { name: "Choose files", exact: true })).toBeEnabled();
      return { page, url, browse: () => browse };
    };
    const first = await open();
    // Delay the second real group until Stop is chosen, without replacing the server.
    await first.page.evaluate(() => {
      const send = WebSocket.prototype.send;
      WebSocket.prototype.send = function (data) {
        if (
          typeof data === "string" &&
          JSON.parse(data).type === "begin_photo_group" &&
          JSON.parse(data).group === 1
        ) {
          (window as unknown as { releaseGroup: () => void }).releaseGroup = () =>
            send.call(this, data);
        } else send.call(this, data);
      };
    });
    await first.page.getByLabel("Choose photo files").setInputFiles(files);
    await first.page.getByRole("button", { name: "Add 3 groups", exact: true }).click();
    await expect
      .poll(() =>
        first.page.evaluate(
          () => typeof (window as unknown as { releaseGroup?: unknown }).releaseGroup,
        ),
      )
      .toBe("function");
    await first.page.getByRole("button", { name: "Stop after this group", exact: true }).click();
    await first.page.evaluate(() =>
      (window as unknown as { releaseGroup: () => void }).releaseGroup(),
    );
    await expect(first.page.getByText("Added: 2", { exact: true })).toBeVisible();
    expect(plans).toHaveLength(1);
    await browser.close();
    await fixture.run("daemon", "stop");
    browser = await chromium.launchPersistentContext(profile);
    const resumed = await open();
    expect(resumed.url.origin).not.toBe(first.url.origin);
    await expect(
      resumed.page.getByRole("button", { name: "Resume import", exact: true }),
    ).toBeVisible();
    await resumed.page.screenshot({
      path: path.join(repository, ".superpowers", "photo-import-resume.png"),
      animations: "disabled",
    });
    // A changed size is refused before any upload, even with the original timestamp.
    const original = await readFile(files[2]!);
    const modified = Buffer.concat([original, Buffer.from([0])]);
    const times = await stat(files[2]!);
    await writeFile(files[2]!, modified);
    await utimes(files[2]!, times.atime, times.mtime);
    const chooserPromise = resumed.page.waitForEvent("filechooser");
    await resumed.page.getByRole("button", { name: "Resume import", exact: true }).click();
    const chooser = await chooserPromise;
    expect(await chooser.element().getAttribute("webkitdirectory")).toBeNull();
    await chooser.setFiles(files);
    await expect(resumed.page.getByRole("alert")).toContainText("differ from the saved import");
    expect(plans).toHaveLength(1);
    await writeFile(files[2]!, original);
    await utimes(files[2]!, times.atime, times.mtime);
    await resumed.page.getByRole("button", { name: "Resume import", exact: true }).click();
    await resumed.page.getByLabel("Choose photo files").setInputFiles(files);
    await resumed.page.getByRole("button", { name: "Add 3 groups", exact: true }).click();
    await expect(resumed.page.getByText("Added: 1", { exact: true })).toBeVisible();
    await expect(resumed.page.getByText("Already in vault: 2", { exact: true })).toBeVisible();
    expect(plans).toHaveLength(2);
    expect(plans[1]).toBe(plans[0]);
    // Wait for real source metadata before opening the final Library view.
    await expect
      .poll(async () => {
        const response = await resumed.page.request.post(
          new URL("/api/v1/photos/assets/query", resumed.url).href,
          {
            headers: {
              "X-Docbank-Web-Session": new URLSearchParams(resumed.url.hash.slice(1)).get(
                "web_session",
              )!,
              Origin: resumed.url.origin,
            },
            data: resumed.browse(),
          },
        );
        expect(response.ok()).toBeTruthy();
        const result = (await response.json()) as PhotoBrowsePage;
        return result.items.filter(
          (row) =>
            row.camera_make === "Synthetic" &&
            row.camera_model === "Camera" &&
            row.capture_time !== null,
        ).length;
      })
      .toBe(3);
    const completed = await open();
    await completed.page.getByRole("button", { name: "Close add photos", exact: true }).click();
    await expect(
      completed.page.getByRole("button", { name: "Expand stack of 3 photos" }),
    ).toBeVisible();
    await completed.page.screenshot({
      path: path.join(repository, ".superpowers", "photo-import-resume-final.png"),
      animations: "disabled",
    });
    await completed.page.locator(".photo-image").first().click();
    await expect(completed.page.getByText("3 selected photos", { exact: true })).toBeVisible();
    await completed.page.getByRole("button", { name: "Cull", exact: true }).click();
    const cull = completed.page.getByRole("dialog", { name: "Cull photos" });
    const decision = completed.page.waitForResponse((response) =>
      response.url().endsWith("/photos/authored/stack"),
    );
    await cull.getByRole("button", { name: "Keep current (Return)", exact: true }).click();
    expect((await decision).ok()).toBe(true);
    await expect(cull.getByRole("button", { name: "Undo", exact: true })).toBeVisible();
    await cull.getByRole("button", { name: "Undo", exact: true }).click();
    await expect(cull.getByText("Edit undone")).toBeVisible();
    await cull.getByRole("button", { name: "Exit cull", exact: true }).click();
    await completed.page.getByRole("button", { name: "Add photos", exact: true }).click();
    await expect(
      completed.page.getByRole("button", { name: "Choose files", exact: true }),
    ).toBeEnabled();
    await expect(
      completed.page.getByRole("button", { name: "Resume import", exact: true }),
    ).toHaveCount(0);
    await completed.page.getByLabel("Choose photo files").setInputFiles(files);
    await expect(
      completed.page.getByRole("button", { name: "Add 3 groups", exact: true }),
    ).toBeVisible();
    expect(plans).toHaveLength(3);
    expect(plans[2]).not.toBe(plans[0]);
  } finally {
    await browser.close();
    await fixture.dispose();
  }
});
