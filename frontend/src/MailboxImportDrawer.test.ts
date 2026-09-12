import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/svelte";
import * as api from "./api.js";
import MailboxImportDrawer from "./MailboxImportDrawer.svelte";
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
it("keeps an unscanned partial import distinct from complete and offers continuation", async () => {
  vi.spyOn(api, "requestJSON").mockResolvedValue([
    {
      id: "job",
      container_id: "source",
      container_sha256: "a".repeat(64),
      settings: { dialect: "mboxrd", destination_id: 1, label_tags: {} },
      state: "partial",
      checkpoint: 2,
      imported: 2,
      rejected: 0,
      retries: 0,
      pending: 0,
      canceled: 0,
      scanned_tail: false,
      collection_id: "collection",
      reason: "Segment limit reached",
    },
  ]);
  render(MailboxImportDrawer, {
    session: "session",
    channel: { uploadMailboxChunk: vi.fn() },
    directory: {
      id: 1,
      name: "root",
      kind: "dir",
      size: 0,
      revision: 1,
      created_at: "2026-09-12T00:00:00Z",
      modified_at: "2026-09-12T00:00:00Z",
    },
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("Unscanned messages remain.")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Continue import" })).toBeTruthy();
  expect(screen.queryByText("Import complete")).toBeNull();
});
