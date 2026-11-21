import { expect, type Locator } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { stripTypeScriptTypes } from "node:module";

// Node does not strip TypeScript in node_modules; kit-ui exports its source.
const source = await readFile(new URL(import.meta.resolve("@kenn-io/kit-ui/utils/color-contrast")), "utf8");
const { contrastRatio, relativeLuminance, WCAG_AA_TEXT }: typeof import("@kenn-io/kit-ui/utils/color-contrast") =
  await import(`data:text/javascript,${encodeURIComponent(stripTypeScriptTypes(source))}`);
export { WCAG_AA_TEXT };

export async function renderedContrast(text: Locator) {
  const { ink, surface } = await text.evaluate((element) => {
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = 1;
    const context = canvas.getContext("2d", { willReadFrequently: true })!;
    const pixel = () => Array.from(context.getImageData(0, 0, 1, 1).data.slice(0, 3));
    const layers: string[] = [];
    for (let node: Element | null = element; node; node = node.parentElement) {
      const background = getComputedStyle(node).backgroundColor;
      layers.unshift(background);
      context.fillStyle = background;
      context.clearRect(0, 0, 1, 1);
      context.fillRect(0, 0, 1, 1);
      if (context.getImageData(0, 0, 1, 1).data[3] === 255) break;
    }
    context.fillStyle = "#fff";
    context.fillRect(0, 0, 1, 1);
    for (const layer of layers) {
      context.fillStyle = layer;
      context.fillRect(0, 0, 1, 1);
    }
    const surface = pixel();
    context.fillStyle = getComputedStyle(element).color;
    context.fillRect(0, 0, 1, 1);
    return { ink: pixel(), surface };
  });
  const hex = (rgb: number[]) => `#${rgb.map((v) => v.toString(16).padStart(2, "0")).join("")}`;
  return { ink, surface, ratio: contrastRatio(hex(ink), hex(surface)), luminance: relativeLuminance(hex(surface)) };
}

// Checks the text color the browser paints against the surface behind it.
export async function expectReadableTone(
  text: Locator,
  theme: "light" | "dark",
  tone: "success" | "warning" | "danger",
) {
  const { ink, surface, ratio, luminance } = await renderedContrast(text);
  const report = `${tone} ink rgb(${ink}) on ${theme} surface rgb(${surface})`;
  expect(luminance > 0.5, report).toBe(theme === "light");
  expect(ratio, report).toBeGreaterThanOrEqual(WCAG_AA_TEXT);
  const [r, g, b] = ink;
  if (tone === "success") expect(g > r && g > b, report).toBe(true);
  else if (tone === "warning") expect(r > b + 40 && g > b, report).toBe(true);
  else expect(r > g && r > b, report).toBe(true);
}
