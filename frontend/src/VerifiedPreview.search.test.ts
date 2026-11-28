import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import VerifiedPreview from "./VerifiedPreview.svelte";
import type { SelectedSource } from "./selectedSource.js";
import * as rendition from "./renditionText.js";

vi.mock("./renditionText.js", () => ({
  resolveRenditionText: vi.fn(),
  readVerifiedRenditionText: vi.fn(),
}));

const source: SelectedSource = {
  kind: "snapshot",
  key: "synthetic-frozen-version",
  nodeID: 7,
  mutationRevision: 3,
  versionID: "11111111-1111-4111-8111-111111111111",
  blobHash: "a".repeat(64),
  size: 50,
  name: "synthetic.pdf",
  path: "/synthetic.pdf",
  mimeType: "application/pdf",
  modifiedAt: "2026-09-11T12:00:00Z",
  observedAt: "2026-09-11T12:00:00Z",
  originalTags: [],
  collectionIDs: [],
};
const ready: rendition.ReadyRenditionText = {
  state: "ready",
  profile: { name: "archive", configuration: "configured", fingerprint: "b".repeat(64) },
  generationID: "c".repeat(64),
  attachmentID: "d".repeat(64),
  buildID: "e".repeat(64),
  artifact: {
    id: "text",
    sha256: "f".repeat(64),
    size: 100,
    mediaType: "text/markdown; charset=utf-8",
  },
};
const props = {
  session: "synthetic",
  source,
  authorizationRevision: 3,
  searchContext: true,
  queryTerms: ["needle"],
  onauthfailure: vi.fn(),
};
const context = `${"Earlier paragraph.\n".repeat(100)}Needle in its original context.\nMore needle evidence.\nLater paragraph.`;

beforeEach(() => {
  vi.mocked(rendition.resolveRenditionText).mockResolvedValue(ready);
  vi.mocked(rendition.readVerifiedRenditionText).mockResolvedValue(context);
  vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("Unexpected native preview request"));
  Object.defineProperty(HTMLElement.prototype, "scrollTo", { configurable: true, value: vi.fn() });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.clearAllMocks();
  Reflect.deleteProperty(HTMLElement.prototype, "scrollTo");
});

it.each([
  "application/pdf",
  "text/plain",
  "text/markdown",
  "image/png",
  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
  "message/rfc822; charset=utf-8",
])("opens %s search content directly in Preview without native rendering", async (mimeType) => {
  render(VerifiedPreview, { ...props, source: { ...source, mimeType } });
  const text = await screen.findByLabelText("Verified text of synthetic.pdf");
  expect(screen.getByRole("tab", { name: "Preview" }).getAttribute("aria-selected")).toBe("true");
  expect(text.textContent).toBe(context);
  expect(text.querySelectorAll("mark[data-source='query']")).toHaveLength(2);
  expect(rendition.resolveRenditionText).toHaveBeenCalledWith(
    "synthetic",
    expect.objectContaining({ versionID: source.versionID, mimeType }),
    3,
    "",
    undefined,
    expect.any(AbortSignal),
  );
  expect(globalThis.fetch).not.toHaveBeenCalled();
});

it("centers query matches, wraps navigation, and handles late query terms without reloading", async () => {
  const view = render(VerifiedPreview, { ...props, queryTerms: [] });
  await screen.findByLabelText("Verified text of synthetic.pdf");
  expect(await screen.findByText(/No literal query highlights/)).toBeTruthy();
  await view.rerender({ ...props });
  await screen.findByText("Search match 1 of 2");
  await waitFor(() => expect(HTMLElement.prototype.scrollTo).toHaveBeenCalled());
  expect(document.querySelector("mark.active")?.textContent).toBe("Needle");
  await fireEvent.click(screen.getByRole("button", { name: "Next search match" }));
  expect(screen.getByText("Search match 2 of 2")).toBeTruthy();
  expect(document.querySelector("mark.active")?.textContent).toBe("needle");
  await fireEvent.click(screen.getByRole("button", { name: "Next search match" }));
  expect(screen.getByText("Search match 1 of 2")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Previous search match" }));
  expect(screen.getByText("Search match 2 of 2")).toBeTruthy();
  expect(rendition.readVerifiedRenditionText).toHaveBeenCalledOnce();
});

it("keeps Find separate from query navigation and reports absent literal matches", async () => {
  const view = render(VerifiedPreview, props);
  const input = await screen.findByLabelText("Find in verified text");
  await fireEvent.input(input, { target: { value: "Later" } });
  expect(document.querySelectorAll("mark[data-source='find']")).toHaveLength(1);
  expect(screen.queryByRole("button", { name: "Next search match" })).toBeNull();
  await fireEvent.input(input, { target: { value: "" } });
  expect(screen.getByText("Search match 1 of 2")).toBeTruthy();
  await view.rerender({ ...props, queryTerms: ["missing literal"] });
  expect(await screen.findByText(/No literal search matches in this text/)).toBeTruthy();
  expect(screen.getByLabelText("Verified text of synthetic.pdf").textContent).toBe(context);
});

it.each([
  ["unconfigured", "Text extraction isn't set up."],
  ["unprocessed", "This version hasn't been processed."],
  ["verified_empty", "Processing verified that this document has no readable text."],
  ["failed", "Text processing failed for this selected version."],
  [
    "historical_unavailable",
    "The rendition recorded for this historical selection is no longer available.",
  ],
] as const)("shows the original preview without a click for %s text", async (state, reason) => {
  vi.mocked(rendition.resolveRenditionText).mockResolvedValue({ ...ready, state });
  render(VerifiedPreview, { ...props, source: { ...source, mimeType: "image/svg+xml" } });
  expect(await screen.findByText(/This file has no preview/i)).toBeTruthy();
  expect(screen.getByText(reason)).toBeTruthy();
  expect(rendition.readVerifiedRenditionText).not.toHaveBeenCalled();
  expect(screen.queryByLabelText("Verified text of synthetic.pdf")).toBeNull();
  expect(globalThis.fetch).not.toHaveBeenCalled();
});

it("explains unprocessed text beside Process and the original preview", async () => {
  vi.mocked(rendition.resolveRenditionText).mockResolvedValue({ ...ready, state: "unprocessed" });
  render(VerifiedPreview, {
    ...props,
    source: { ...source, mimeType: "image/svg+xml" },
    onprocess: vi.fn(),
  });
  expect(await screen.findByText(/This file has no preview/i)).toBeTruthy();
  expect(screen.getByText("This version hasn't been processed.")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Process…" })).toBeTruthy();
});

it("keeps the reader's scroll position when returning to Preview", async () => {
  render(VerifiedPreview, props);
  await screen.findByText("Search match 1 of 2");
  const scrollTo = vi.mocked(HTMLElement.prototype.scrollTo);
  await waitFor(() => expect(scrollTo).toHaveBeenCalled());
  scrollTo.mockClear();
  await fireEvent.click(screen.getByRole("tab", { name: "Text" }));
  await screen.findAllByLabelText("Verified text of synthetic.pdf");
  await fireEvent.click(screen.getByRole("tab", { name: "Preview" }));
  await Promise.resolve();
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(scrollTo).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Next search match" }));
  await waitFor(() => expect(scrollTo).toHaveBeenCalled());
});

it("reads the verified text once when the reader then opens the Text tab", async () => {
  render(VerifiedPreview, props);
  await screen.findByText("Search match 1 of 2");
  await fireEvent.click(screen.getByRole("tab", { name: "Text" }));
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(rendition.resolveRenditionText).toHaveBeenCalledOnce();
  expect(rendition.readVerifiedRenditionText).toHaveBeenCalledOnce();
  const [text] = screen.getAllByLabelText("Verified text of synthetic.pdf");
  expect(screen.getAllByLabelText("Verified text of synthetic.pdf")).toHaveLength(1);
  expect(text!.textContent).toBe(context);
  expect(text!.closest(".verified-text")?.classList.contains("search-context")).toBe(false);
  expect(screen.queryByRole("button", { name: "Next search match" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Show original preview" })).toBeNull();
});

it("restores Preview's scroll position after reading the same text in Text", async () => {
  render(VerifiedPreview, props);
  await screen.findByText("Search match 1 of 2");
  const text = screen.getByLabelText("Verified text of synthetic.pdf");
  let scrollTop = 345;
  Object.defineProperty(text, "scrollTop", {
    configurable: true,
    get: () => scrollTop,
    set: (value: number) => (scrollTop = value),
  });
  await fireEvent.click(screen.getByRole("tab", { name: "Text" }));
  // The Text layout lets the page scroll instead, which resets the box.
  scrollTop = 0;
  await fireEvent.click(screen.getByRole("tab", { name: "Preview" }));
  await waitFor(() => expect(scrollTop).toBe(345));
});

it("keeps the active match when the same query terms arrive again", async () => {
  const view = render(VerifiedPreview, { ...props, queryKey: "same" });
  await screen.findByText("Search match 1 of 2");
  await fireEvent.click(screen.getByRole("button", { name: "Next search match" }));
  expect(screen.getByText("Search match 2 of 2")).toBeTruthy();
  const scrollTo = vi.mocked(HTMLElement.prototype.scrollTo);
  await waitFor(() => expect(scrollTo).toHaveBeenCalled());
  scrollTo.mockClear();
  await view.rerender({ ...props, queryKey: "same", queryTerms: ["needle"] });
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(screen.getByText("Search match 2 of 2")).toBeTruthy();
  expect(scrollTo).not.toHaveBeenCalled();
});

it("requests the original image for a name-only match without extracted text", async () => {
  vi.mocked(rendition.resolveRenditionText).mockResolvedValue({ ...ready, state: "unprocessed" });
  render(VerifiedPreview, { ...props, source: { ...source, mimeType: "image/jpeg" } });
  await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled());
  expect(String(vi.mocked(globalThis.fetch).mock.calls[0]![0])).toContain("web-download");
  expect(screen.queryByLabelText("Verified text of synthetic.pdf")).toBeNull();
});

it("returns to search context when a result is reselected or a new query is accepted", async () => {
  const other = { ...source, key: "other", versionID: "22222222-2222-4222-8222-222222222222" };
  const view = render(VerifiedPreview, { ...props, queryKey: "first" });
  await screen.findByLabelText("Verified text of synthetic.pdf");
  await fireEvent.click(screen.getByRole("button", { name: "Show original preview" }));
  expect(await screen.findByRole("button", { name: "Show search context" })).toBeTruthy();
  expect(screen.queryByLabelText("Verified text of synthetic.pdf")).toBeNull();

  await view.rerender({ ...props, queryKey: "first", source: other });
  await screen.findByLabelText("Verified text of synthetic.pdf");
  await view.rerender({ ...props, queryKey: "first" });
  expect(await screen.findByLabelText("Verified text of synthetic.pdf")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Show search context" })).toBeNull();

  await fireEvent.click(screen.getByRole("button", { name: "Show original preview" }));
  await screen.findByRole("button", { name: "Show search context" });
  await view.rerender({ ...props, queryKey: "second" });
  expect(await screen.findByLabelText("Verified text of synthetic.pdf")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Show search context" })).toBeNull();
});

it("keeps the original preview choice when the selection's metadata revision changes", async () => {
  const view = render(VerifiedPreview, { ...props, queryKey: "first" });
  await screen.findByLabelText("Verified text of synthetic.pdf");
  await fireEvent.click(screen.getByRole("button", { name: "Show original preview" }));
  await screen.findByRole("button", { name: "Show search context" });
  await view.rerender({ ...props, queryKey: "first", authorizationRevision: 4 });
  expect(screen.getByRole("button", { name: "Show search context" })).toBeTruthy();
  expect(screen.queryByLabelText("Verified text of synthetic.pdf")).toBeNull();
});

it("waits for query highlights before saying none are available", async () => {
  const view = render(VerifiedPreview, { ...props, queryTerms: [], queryHighlightsPending: true });
  await screen.findByLabelText("Verified text of synthetic.pdf");
  expect(screen.queryByText(/No literal query highlights/)).toBeNull();
  expect(screen.queryByText(/No literal search matches/)).toBeNull();
  await view.rerender({ ...props, queryTerms: [], queryHighlightsPending: false });
  expect(await screen.findByText(/No literal query highlights/)).toBeTruthy();
});

it("returns the same result to Preview when a new search is accepted", async () => {
  const view = render(VerifiedPreview, { ...props, queryKey: "first" });
  await screen.findByLabelText("Verified text of synthetic.pdf");
  await fireEvent.click(screen.getByRole("tab", { name: "Duplicates" }));
  await view.rerender({ ...props, queryKey: "first", queryTerms: ["needle"] });
  expect(screen.getByRole("tab", { name: "Duplicates" }).getAttribute("aria-selected")).toBe(
    "true",
  );
  await view.rerender({ ...props, queryKey: "second" });
  await waitFor(() =>
    expect(screen.getByRole("tab", { name: "Preview" }).getAttribute("aria-selected")).toBe("true"),
  );
});

it("clears Find when a new query is accepted for the same document", async () => {
  const view = render(VerifiedPreview, { ...props, queryKey: "first" });
  const input = await screen.findByLabelText<HTMLInputElement>("Find in verified text");
  await fireEvent.input(input, { target: { value: "Later" } });
  expect(document.querySelectorAll("mark[data-source='find']")).toHaveLength(1);
  await view.rerender({ ...props, queryKey: "second", queryTerms: ["evidence"] });
  await screen.findByText("Search match 1 of 1");
  expect(document.querySelectorAll("mark[data-source='find']")).toHaveLength(0);
  expect(document.querySelector("mark.active")?.textContent).toBe("evidence");
  expect(screen.getByLabelText<HTMLInputElement>("Find in verified text").value).toBe("");
});

it("opens Preview when a different search result is selected after another tab", async () => {
  const other = { ...source, key: "other", versionID: "22222222-2222-4222-8222-222222222222" };
  const view = render(VerifiedPreview, { ...props, activeTab: "text" });
  await waitFor(() =>
    expect(screen.getByRole("tab", { name: "Preview" }).getAttribute("aria-selected")).toBe("true"),
  );
  await fireEvent.click(screen.getByRole("tab", { name: "Duplicates" }));
  expect(screen.getByRole("tab", { name: "Duplicates" }).getAttribute("aria-selected")).toBe(
    "true",
  );
  await view.rerender({ ...props, source: other });
  await waitFor(() =>
    expect(screen.getByRole("tab", { name: "Preview" }).getAttribute("aria-selected")).toBe("true"),
  );
});

it("keeps the ordinary Text tab layout and scrolls Find matches only when moved", async () => {
  const scrollIntoView = vi.fn();
  Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
    configurable: true,
    value: scrollIntoView,
  });
  try {
    render(VerifiedPreview, { ...props, searchContext: false, activeTab: "text" });
    const text = await screen.findByLabelText("Verified text of synthetic.pdf");
    expect(text.closest(".verified-text")?.classList.contains("search-context")).toBe(false);
    await fireEvent.input(screen.getByLabelText("Find in verified text"), {
      target: { value: "paragraph" },
    });
    await Promise.resolve();
    expect(HTMLElement.prototype.scrollTo).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole("button", { name: "Next text match" }));
    await waitFor(() => expect(scrollIntoView).toHaveBeenCalledWith({ block: "center" }));
    expect(HTMLElement.prototype.scrollTo).not.toHaveBeenCalled();
  } finally {
    Reflect.deleteProperty(HTMLElement.prototype, "scrollIntoView");
  }
});

it("does not expose text after verification fails", async () => {
  vi.mocked(rendition.readVerifiedRenditionText).mockRejectedValue(new Error("SHA-256 mismatch"));
  render(VerifiedPreview, props);
  expect((await screen.findByRole("alert")).textContent).toContain("SHA-256 mismatch");
  expect(screen.queryByLabelText("Verified text of synthetic.pdf")).toBeNull();
});

it("discards delayed text when the selected frozen version changes", async () => {
  let complete!: (text: string) => void;
  vi.mocked(rendition.readVerifiedRenditionText)
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          complete = resolve;
        }),
    )
    .mockResolvedValueOnce("New needle context");
  const view = render(VerifiedPreview, props);
  await waitFor(() => expect(rendition.readVerifiedRenditionText).toHaveBeenCalledOnce());
  const oldSignal = vi.mocked(rendition.readVerifiedRenditionText).mock.calls[0]![4];
  await view.rerender({
    ...props,
    source: { ...source, key: "next", versionID: "22222222-2222-4222-8222-222222222222" },
  });
  await screen.findByText("New", { exact: false });
  complete("Old needle context must never reappear");
  await waitFor(() => expect(oldSignal.aborted).toBe(true));
  expect(screen.getByLabelText("Verified text of synthetic.pdf").textContent).toBe(
    "New needle context",
  );
});

it("centers matches by scrolling only their text container", async () => {
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    return { top: this.tagName === "MARK" ? 1100 : 100 } as DOMRect;
  });
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLElement.prototype, "offsetHeight", "get").mockReturnValue(40);
  vi.spyOn(HTMLElement.prototype, "scrollTop", "get").mockReturnValue(70);
  render(VerifiedPreview, props);
  await screen.findByText("Search match 1 of 2");
  const text = screen.getByLabelText("Verified text of synthetic.pdf");
  const scrollTo = vi.mocked(HTMLElement.prototype.scrollTo);
  await waitFor(() => {
    expect(scrollTo).toHaveBeenCalledWith({ top: 890 });
    const calls = scrollTo.mock.calls as unknown as [ScrollToOptions][];
    const callIndex = calls.findIndex(([options]) => options?.top === 890);
    expect(scrollTo.mock.contexts[callIndex]).toBe(text);
  });
});

it("asks for a processing profile when the text selection is ambiguous", async () => {
  vi.mocked(rendition.resolveRenditionText).mockResolvedValue({
    ...ready,
    state: "profile_required",
  });
  render(VerifiedPreview, props);
  expect(
    await screen.findByText("Choose a processing profile before reading rendition text."),
  ).toBeTruthy();
  expect(screen.queryByText("This version hasn't been processed.")).toBeNull();
});
