import type { Page } from "@playwright/test";
import path from "node:path";

export async function captureThemes(
  page: Page,
  output: string,
  name: string,
  clip?: { x: number; y: number; width: number; height: number },
) {
  for (const theme of ["light", "dark"]) {
    await page.evaluate(async (value) => {
      localStorage.setItem("docbank-theme", value);
      document.documentElement.classList.toggle("dark", value === "dark");
      await document.fonts.ready;
    }, theme);
    await page.screenshot({
      path: path.join(output, `${name}-${theme}.png`),
      clip,
      animations: "disabled",
    });
  }
}
