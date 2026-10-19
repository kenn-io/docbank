import { expect, test } from "@playwright/test";
import { mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { photoFixture, repository } from "./photo-fixture.js";

for (const prior of [false, true]) {
  test(`rejected folder plan preserves saved progress, prior selection: ${prior}`, async ({
    page,
    context,
  }) => {
    page.setDefaultTimeout(15_000);
    await mkdir(path.join(repository, ".superpowers"), { recursive: true });
    const workspace = await mkdtemp(path.join(repository, ".superpowers", "import-rejection-"));
    const fixture = photoFixture(workspace);
    const errors: { type: string; code?: string; status?: number }[] = [];
    page.on("websocket", (socket) =>
      socket.on("framereceived", ({ payload }) => {
        if (typeof payload !== "string") return;
        const reply = JSON.parse(payload);
        if (reply.type === "error") errors.push(reply);
      }),
    );
    try {
      const obstruction = path.join(workspace, "obstruction.txt");
      await writeFile(obstruction, "Synthetic destination obstruction");
      await fixture.run("add", obstruction, "--dest", "/");
      const url = new URL(await fixture.run("web", "--no-browser"));
      url.pathname = "/photos";
      await page.goto(url.href);
      const open = async () => {
        await page.getByRole("button", { name: "Add photos", exact: true }).first().click();
        await expect(page.getByRole("button", { name: "Choose files", exact: true })).toBeEnabled();
      };
      await open();
      const profile = (await context.cookies()).find(
        (cookie) => cookie.name === "docbank_photo_profile",
      )!;
      const savedPath = path.join(
        workspace,
        "vault",
        "web-photo-imports",
        profile.value + ".jsonl",
      );
      const selectionA = path.join(workspace, "selection-a.jpg");
      const selectionB = path.join(workspace, "selection-b.jpg");
      await writeFile(selectionA, "synthetic selection A");
      await writeFile(selectionB, "synthetic selection B");
      let saved: string | undefined;
      if (prior) {
        await page.getByLabel("Choose photo files").setInputFiles(selectionA);
        await expect(page.getByRole("button", { name: "Add 1 group", exact: true })).toBeEnabled();
        saved = await readFile(savedPath, "utf8");
        await page.getByRole("button", { name: "Close add photos", exact: true }).click();
        await open();
      }
      await page.getByLabel("Destination", { exact: true }).fill("/obstruction.txt/child");
      await page.getByLabel("Choose photo files").setInputFiles(selectionB);
      if (prior) await page.getByRole("button", { name: "Replace import", exact: true }).click();
      await expect(page.getByRole("alert")).toContainText(/not a directory/i);
      await expect.poll(() => errors.find((reply) => reply.code === "not_dir")?.status).toBe(422);
      await expect(
        page.getByText("Keep this tab open until the import finishes.", { exact: true }),
      ).toHaveCount(0);
      for (const theme of prior ? ["dark", "light"] : ["dark"]) {
        await page.evaluate((value) => {
          localStorage.setItem("docbank-theme", value);
          document.documentElement.classList.toggle("dark", value === "dark");
        }, theme);
        await page.screenshot({
          clip: prior ? { x: 600, y: 0, width: 840, height: 360 } : undefined,
          path: path.join(
            repository,
            ".superpowers",
            prior ? `photo-import-rejection-${theme}.png` : "photo-import-first-rejection.png",
          ),
          animations: "disabled",
        });
      }
      if (prior) expect(await readFile(savedPath, "utf8")).toBe(saved);
      else await expect(readFile(savedPath, "utf8")).rejects.toThrow(/ENOENT/);
      await page.getByRole("button", { name: "Close add photos", exact: true }).click();
      await open();
      await expect(page.getByRole("button", { name: "Resume import", exact: true })).toHaveCount(
        prior ? 1 : 0,
      );
      if (prior) {
        await page.getByRole("button", { name: "Resume import", exact: true }).click();
        await page.getByLabel("Choose photo files").setInputFiles(selectionA);
        await expect(page.getByRole("button", { name: "Add 1 group", exact: true })).toBeEnabled();
        const resumed = (await readFile(savedPath, "utf8")).split("\n")[1]!;
        expect(JSON.parse(resumed).sourceID).toBe(JSON.parse(saved!.split("\n")[1]!).sourceID);
        await page.getByRole("button", { name: "Close add photos", exact: true }).click();
        const lines = (await readFile(savedPath, "utf8")).trim().split("\n");
        lines[1] = JSON.stringify({ ...JSON.parse(lines[1]!), unknown: true });
        await writeFile(savedPath, lines.join("\n") + "\n");
        const freshURL = new URL(await fixture.run("web", "--no-browser"));
        freshURL.pathname = "/photos";
        await page.goto(freshURL.href);
        await page.getByRole("button", { name: "Add photos", exact: true }).first().click();
        await expect(page.getByRole("alert")).toContainText("unsupported or malformed format");
        await expect(
          page.getByRole("button", { name: "Choose files", exact: true }),
        ).toBeDisabled();
        await page.screenshot({
          path: path.join(repository, ".superpowers", "photo-import-format-blocked.png"),
          animations: "disabled",
        });
      }
    } finally {
      await fixture.dispose();
    }
  });
}
