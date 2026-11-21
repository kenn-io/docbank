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
  "unconfigured",
  "unprocessed",
  "verified_empty",
  "failed",
  "historical_unavailable",
] as const)(
  "offers an explicit visual fallback for %s text without preparing a PDF",
  async (state) => {
    vi.mocked(rendition.resolveRenditionText).mockResolvedValue({ ...ready, state });
    render(VerifiedPreview, props);
    expect(await screen.findByRole("button", { name: "Show original preview" })).toBeTruthy();
    expect(rendition.readVerifiedRenditionText).not.toHaveBeenCalled();
    expect(screen.queryByLabelText("Verified text of synthetic.pdf")).toBeNull();
    expect(globalThis.fetch).not.toHaveBeenCalled();
  },
);

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
