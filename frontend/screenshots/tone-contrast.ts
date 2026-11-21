import { expect, type Locator } from "@playwright/test";

// Checks the text color the browser actually paints against the surface
// behind it, so a tone that reads too faintly in either theme fails.
export async function expectReadableTone(
  text: Locator,
  theme: "light" | "dark",
  tone: "success" | "warning",
) {
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
  const luminance = (rgb: number[]) => {
    const [r, g, b] = rgb.map((v) => {
      const c = v / 255;
      return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
    });
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
  };
  const [lighter, darker] = [luminance(ink), luminance(surface)].sort((a, b) => b - a);
  const report = `${tone} ink rgb(${ink}) on ${theme} surface rgb(${surface})`;
  expect(luminance(surface) > 0.5, report).toBe(theme === "light");
  expect((lighter + 0.05) / (darker + 0.05), report).toBeGreaterThanOrEqual(4.5);
  const [r, g, b] = ink;
  if (tone === "success") expect(g > r && g > b, report).toBe(true);
  else expect(r > b + 40 && g > b, report).toBe(true);
}
