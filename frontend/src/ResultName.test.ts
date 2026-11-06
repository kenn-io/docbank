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

it("only uses a validity tooltip when it adds information", () => {
  const sameReason = render(ResultName, {
    name: "lease-2024.pdf",
    folder: "/leases",
    excerpt: "renewal excerpt",
    validityReason: "expired",
  });
  const reason = sameReason.container.querySelector(".validity-reason");
  expect(reason?.getAttribute("title")).toBeNull();
  const excerpt = screen.getByText("renewal excerpt");
  const text = excerpt.parentElement!;
  expect(text.querySelector(".folder")?.textContent).toBe("/leases");
  expect(reason?.parentElement).toBe(text);
  expect(excerpt.nextElementSibling).toBe(reason);
  sameReason.unmount();

  const detailedTitle = "superseded by /leases/2025.pdf; /leases/2026/renewal.pdf";
  const moreDetail = render(ResultName, {
    name: "lease-2024.pdf",
    validityReason: "superseded by lease-2025.pdf",
    validityReasonTitle: detailedTitle,
  });
  expect(moreDetail.container.querySelector(".validity-reason")?.getAttribute("title")).toBe(
    detailedTitle,
  );
});
