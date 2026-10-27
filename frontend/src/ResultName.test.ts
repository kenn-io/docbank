import { afterEach, expect, it } from "vite-plus/test";
import { cleanup, render, screen } from "@testing-library/svelte";
import ResultName from "./ResultName.svelte";

afterEach(cleanup);

it("shows the name once with its folder beneath", () => {
  const { container } = render(ResultName, {
    name: "invoice-1.md",
    folder: "/clients/acme/invoices",
    mime: "text/markdown",
  });
  expect(screen.getByText("invoice-1.md")).toBeTruthy();
  expect(container.querySelector(".folder")?.textContent).toBe("/clients/acme/invoices");
  expect(container.textContent?.match(/invoice-1\.md/g)).toHaveLength(1);
});

it("adds a matched excerpt only when given one", () => {
  const view = render(ResultName, { name: "a.txt", folder: "/x" });
  expect(view.container.querySelector(".excerpt")).toBeNull();
  view.unmount();
  const withExcerpt = render(ResultName, { name: "a.txt", folder: "/x", excerpt: "net 30 terms" });
  expect(withExcerpt.container.querySelector(".excerpt")?.textContent).toBe("net 30 terms");
});
