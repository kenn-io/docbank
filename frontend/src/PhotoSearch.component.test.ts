import * as actions from "./action-events.js";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoSearch from "./PhotoSearch.svelte";
import { Photos } from "./photos.svelte.js";
import { tick } from "svelte";

vi.mock("./action-events.js", () => ({
  reportAction: vi.fn(),
  reportSearch: vi.fn(),
  reportFailure: vi.fn(),
}));

beforeEach(() => {
  vi.mocked(actions.reportAction).mockClear();
  vi.mocked(actions.reportSearch).mockClear();
  vi.mocked(actions.reportFailure).mockClear();
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

it.each(["simple", "advanced"] as const)("submits %s text", async (syntax) => {
  const photos = new Photos("synthetic", vi.fn());
  photos.baseQuery = {
    ...photos.baseQuery,
    syntax,
    filters: { cameras: ["Synthetic camera"] },
  };
  photos.query = {
    ...photos.baseQuery,
    filters: {
      cameras: ["Synthetic camera"],
      capture_after: "2024-06-01",
      capture_before: "2024-07-01",
    },
  };
  photos.date = "2024-06";
  const submit = vi.spyOn(photos, "setQuery").mockResolvedValue(undefined);
  render(PhotoSearch, { photos });
  const input = screen.getByRole("searchbox", { name: "Search photos" }) as HTMLInputElement;
  await fireEvent.input(input, { target: { value: "draft" } });
  await fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
  expect(input.value).toBe("");
  expect(submit).not.toHaveBeenCalled();
  await fireEvent.input(input, { target: { value: "camera" } });
  await fireEvent.submit(input.closest("form")!);
  expect(submit).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({
      text: "camera",
      syntax,
      filters: { cameras: ["Synthetic camera"] },
    }),
  );
  photos.baseQuery = { ...photos.baseQuery, text: "camera" };
  await fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
  expect(submit).toHaveBeenLastCalledWith(expect.objectContaining({ text: "", syntax }));
  photos.dispose();
});

it("restores applied text and mode when staged metadata prevents changing the search", async () => {
  const photos = new Photos("synthetic", vi.fn());
  photos.baseQuery = { ...photos.baseQuery, text: "camera" };
  photos.query = photos.baseQuery;
  photos.authored.stage({ caption: "Unapplied" });
  render(PhotoSearch, { photos });
  const input = screen.getByRole("searchbox", { name: "Search photos" }) as HTMLInputElement;
  await fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
  expect(input.value).toBe("camera");
  expect(photos.baseQuery.text).toBe("camera");
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Plain words" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Query syntax" }));
  expect(photos.baseQuery.syntax).toBe("simple");
  expect(screen.getByRole("combobox", { name: "Search mode: Plain words" })).toBeTruthy();
  photos.dispose();
});

it("preserves draft text and mode across facet changes and syncs applied values", async () => {
  const photos = new Photos("synthetic", vi.fn());
  render(PhotoSearch, { photos });
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Plain words" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Query syntax" }));
  const input = screen.getByRole("searchbox", { name: "Search photos" }) as HTMLInputElement;
  await fireEvent.input(input, { target: { value: "rating:5" } });
  photos.baseQuery = photos.query = {
    ...photos.baseQuery,
    filters: { cameras: ["Synthetic camera"] },
  };
  await tick();
  expect(input.value).toBe("rating:5");
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Search mode: Query syntax" })).toBeTruthy(),
  );
  photos.baseQuery = photos.query = { ...photos.baseQuery, filters: {} };
  await tick();
  expect(input.value).toBe("rating:5");
  photos.baseQuery = photos.query = { ...photos.baseQuery, text: "changed" };
  await waitFor(() => expect(input.value).toBe("changed"));
  photos.baseQuery = photos.query = { ...photos.baseQuery, syntax: "advanced" };
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Search mode: Query syntax" })).toBeTruthy(),
  );
  photos.baseQuery = photos.query = { ...photos.baseQuery, syntax: "simple" };
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Search mode: Plain words" })).toBeTruthy(),
  );
  photos.dispose();
});

it("disables search and its mode while a photo is hiding", async () => {
  const photos = new Photos("synthetic", vi.fn());
  render(PhotoSearch, { photos });
  const input = screen.getByRole("searchbox", { name: "Search photos" }) as HTMLInputElement;
  photos.hiding = true;
  await waitFor(() => expect(input.disabled).toBe(true));
  expect(
    screen.getByRole("combobox", { name: "Search mode: Plain words" }).hasAttribute("disabled"),
  ).toBe(true);
  photos.dispose();
});

it("applies the draft when the search mode changes", async () => {
  const photos = new Photos("synthetic", vi.fn());
  const submit = vi.spyOn(photos, "setQuery").mockResolvedValue(undefined);
  render(PhotoSearch, { photos });
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Plain words" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Query syntax" }));
  expect(submit).not.toHaveBeenCalled();
  await fireEvent.input(screen.getByRole("searchbox", { name: "Search photos" }), {
    target: { value: "rating:5" },
  });
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Query syntax" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Plain words" }));
  expect(submit).toHaveBeenLastCalledWith(
    expect.objectContaining({ text: "rating:5", syntax: "simple" }),
  );
  photos.baseQuery = photos.query = { ...photos.baseQuery, text: "camera" };
  await tick();
  submit.mockClear();
  await fireEvent.input(screen.getByRole("searchbox", { name: "Search photos" }), {
    target: { value: "" },
  });
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Plain words" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Query syntax" }));
  expect(submit).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({ text: "", syntax: "advanced" }),
  );
  photos.dispose();
});

it("reports Photos search outcomes for explicit submissions", async () => {
  const fetcher = vi.fn().mockResolvedValue(new Response("unavailable", { status: 500 }));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  render(PhotoSearch, { photos });
  const search = screen.getByRole("searchbox", { name: "Search photos" });
  fetcher.mockResolvedValueOnce(Response.json({ items: [], total: 0, facets: [] }));
  await fireEvent.input(search, { target: { value: "Nikon" } });
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() =>
    expect(actions.reportSearch).toHaveBeenCalledExactlyOnceWith("scoped", "text", true),
  );
  vi.mocked(actions.reportSearch).mockClear();
  await fireEvent.input(search, { target: { value: "Canon" } });
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() =>
    expect(actions.reportFailure).toHaveBeenCalledExactlyOnceWith(
      "scoped",
      "search",
      expect.objectContaining({ status: 500 }),
    ),
  );
  vi.mocked(actions.reportFailure).mockClear();
  fetcher.mockResolvedValueOnce(Response.json({ items: [], total: 0, facets: [] }));
  await fireEvent.input(search, { target: { value: "   " } });
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() => expect(photos.loading).toBe(false));
  expect(actions.reportSearch).not.toHaveBeenCalled();
  expect(actions.reportFailure).not.toHaveBeenCalled();

  fetcher.mockClear();
  await fireEvent.input(search, { target: { value: "q".repeat(8193) } });
  await fireEvent.submit(search.closest("form")!);
  expect(photos.error).toContain("8192");
  expect(fetcher).not.toHaveBeenCalled();
  expect(actions.reportSearch).not.toHaveBeenCalled();
  await waitFor(() =>
    expect(actions.reportFailure).toHaveBeenCalledExactlyOnceWith(
      "scoped",
      "search",
      expect.objectContaining({ message: expect.stringContaining("8192") }),
    ),
  );
  vi.mocked(actions.reportFailure).mockClear();

  fetcher.mockImplementationOnce(
    (_url: string, init: RequestInit) =>
      new Promise((_resolve, reject) =>
        init.signal!.addEventListener("abort", () => reject(init.signal!.reason)),
      ),
  );
  await fireEvent.input(search, { target: { value: "Fuji" } });
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() => expect(photos.loading).toBe(true));
  await act(() => photos.dispose());
  expect(actions.reportSearch).not.toHaveBeenCalled();
  expect(actions.reportFailure).not.toHaveBeenCalled();
});

it.each([
  { hidden: true, code: "hidden_locked", handled: true },
  { hidden: false, code: "forbidden", handled: false },
])(
  "reports submitted search failures only when access cleanup does not handle them, hidden=$hidden",
  async ({ hidden, code, handled }) => {
    const onauth = vi.fn(() => photos.dispose());
    const photos = new Photos("scoped", onauth, hidden);
    photos.started = true;
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(Response.json({ code, detail: "Access denied" }, { status: 403 })),
    );
    render(PhotoSearch, { photos });
    const search = screen.getByRole("searchbox", { name: "Search photos" });
    await fireEvent.input(search, { target: { value: "harbor" } });
    await fireEvent.submit(search.closest("form")!);
    if (handled) {
      await waitFor(() => expect(onauth).toHaveBeenCalledOnce());
      expect(actions.reportFailure).not.toHaveBeenCalled();
    } else {
      await waitFor(() =>
        expect(actions.reportFailure).toHaveBeenCalledExactlyOnceWith(
          "scoped",
          "search",
          expect.objectContaining({ status: 403, code }),
        ),
      );
      expect(onauth).not.toHaveBeenCalled();
    }
    expect(actions.reportSearch).not.toHaveBeenCalled();
    photos.dispose();
  },
);
