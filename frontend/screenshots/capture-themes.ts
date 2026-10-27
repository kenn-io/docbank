import type { Page } from "@playwright/test";
import path from "node:path";

export async function captureThemes(
  page: Page,
  output: string,
  name: string,
  clip?: { x: number; y: number; width: number; height: number },
) {
  for (const theme of ["light", "dark"] as const) {
    await captureTheme(page, output, name, theme, clip);
  }
}

export async function captureTheme(
  page: Page,
  output: string,
  name: string,
  theme: "light" | "dark",
  clip?: { x: number; y: number; width: number; height: number },
) {
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
