import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import * as api from "./api-transport.js";
import * as mailbox from "./mailbox.js";
import MailboxImportDrawer from "./MailboxImportDrawer.svelte";

const job = {
  id: "job", container_id: "source", container_sha256: "a".repeat(64),
  settings: { dialect: "mboxrd", destination_id: 1, label_tags: {} },
  state: "partial", checkpoint: 2, imported: 2, rejected: 0,
  pending: 0, canceled: 0, scanned_tail: false, collection_id: "collection",
  reason: "Segment limit reached", started_at: "2026-09-12T00:00:00Z",
};
const props = {
  session: "session", channel: { uploadMailboxChunk: vi.fn() },
  directory: { id: 1, name: "root", kind: "dir" as const, size: 0, revision: 1, created_at: "2026-09-12T00:00:00Z", modified_at: "2026-09-12T00:00:00Z" },
  onclose: vi.fn(), oncomplete: vi.fn(), onauthfailure: vi.fn(),
};
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
});
it("keeps an unscanned partial import distinct from complete and offers continuation", async () => {
  vi.spyOn(api, "sessionJSON").mockResolvedValue([job]);
  render(MailboxImportDrawer, props);
  expect(await screen.findByText("Unscanned messages remain.")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Continue import" })).toBeTruthy();
  expect(screen.queryByText("Import complete")).toBeNull();
});
it("shows the most recently started import regardless of its identifier", async () => {
  vi.spyOn(api, "sessionJSON").mockResolvedValue([
    { ...job, id: "a-newest", started_at: "2026-09-13T00:00:00Z" },
    { ...job, id: "z-oldest" },
  ]);
  render(MailboxImportDrawer, props);
  expect(await screen.findByText("Import a-newest")).toBeTruthy();
});
it.each([{ jobs: [] }, { jobs: [{ ...job, state: "complete" }] }])("does not poll without an active import: $jobs", async ({ jobs }) => {
  vi.useFakeTimers();
  const request = vi.spyOn(api, "sessionJSON").mockResolvedValue(jobs);
  render(MailboxImportDrawer, props);
  await vi.advanceTimersByTimeAsync(5000);
  expect(request).toHaveBeenCalledTimes(1);
});
it("polls the selected job until it finishes and then stops", async () => {
  vi.useFakeTimers();
  const request = vi.spyOn(api, "sessionJSON")
    .mockResolvedValueOnce([{ ...job, state: "running" }])
    .mockResolvedValue([{ ...job, state: "complete", scanned_tail: true }]);
  const oncomplete = vi.fn();
  render(MailboxImportDrawer, { ...props, oncomplete });
  await vi.advanceTimersByTimeAsync(1000);
  await tick();
  expect(screen.getByText("Import complete")).toBeTruthy();
  expect(oncomplete).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(5000);
  expect(request).toHaveBeenCalledTimes(2);
});
it("keeps a separate import identity when starting again after an error", async () => {
  const requests: { id: string; container_id: string }[] = [];
  vi.spyOn(mailbox, "uploadMailboxArchive").mockResolvedValue({ id: "source", sha256: "a".repeat(64), size: 3, format: "mbox", state: "sealed", chunks: [], manifest_sha256: "b".repeat(64) });
  vi.spyOn(api, "sessionJSON").mockImplementation(async (path, options) => {
    if (path.endsWith("/preview")) return { dialect: "mboxrd", entry_count: 1, entries: [], samples: [], has_more: false };
    if (path.endsWith("/jobs")) {
      requests.push(JSON.parse(options.body as string));
      throw new Error("Try starting again");
    }
    return [];
  });
  render(MailboxImportDrawer, props);
  await fireEvent.change(screen.getByLabelText("Choose MBOX or Takeout ZIP"), { target: { files: [new File(["abc"], "mail.mbox")] } });
  await fireEvent.click(screen.getByRole("button", { name: "Upload and preview" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Import messages" }));
  await screen.findByRole("alert");
  await fireEvent.click(screen.getByRole("button", { name: "Import messages" }));
  expect(requests).toHaveLength(2);
  expect(requests[0].id).not.toBe("source");
  expect(requests[0]).toEqual(requests[1]);
});
