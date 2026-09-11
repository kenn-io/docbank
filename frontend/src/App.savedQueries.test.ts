import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import App from "./App.svelte";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it("restores a full query beside sign-in without running it as empty-text folder browsing", async () => {
  const query = { v: 1, text: "", syntax: "advanced", mode: "hybrid", filters: { extensions: ["pdf"], no_tags: true }, sort: { field: "size", direction: "desc" } };
  history.replaceState(null, "", `/#web_session=secret&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  const root = { id: 1, name: "", kind: "dir", path: "/", revision: 1, size: 0, created_at: "2026-01-01T00:00:00Z", modified_at: "2026-01-01T00:00:00Z" };
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    const value = url.includes("/path?") ? root : url.includes("/children?") ? { directory: root, items: [], total: 0, limit: 1000, offset: 0 }
      : { items: [], total: 0, limit: url.includes("saved-queries") ? 100 : 1000, offset: 0 };
    return new Response(JSON.stringify(value));
  });
  render(App);
  await screen.findByRole("dialog", { name: "Saved queries and highlights" });
  expect(JSON.parse((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value)).toEqual(query);
  expect(location.hash).not.toContain("web_session");
  expect(location.hash).not.toContain("proof");
  expect(fetch.mock.calls.some(([url]) => String(url).includes("/search"))).toBe(false);
  await fireEvent.click(screen.getByRole("button", { name: "Close" }));
  await fireEvent.click(screen.getByRole("button", { name: "Saved queries and highlights" }));
  expect(JSON.parse((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value)).toEqual(query);
  await fireEvent.click(screen.getByRole("button", { name: "Close" }));
  await fireEvent.click(screen.getByRole("button", { name: "Discard query draft" }));
  await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), { target: { value: "x".repeat(8193) } });
  await fireEvent.click(screen.getByRole("button", { name: "Saved queries and highlights" }));
  expect((await screen.findByRole("alert")).textContent).toContain("8192");
  expect(screen.queryByRole("dialog", { name: "Saved queries and highlights" })).toBeNull();
});
