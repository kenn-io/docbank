import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import TagRunNotice from "./TagRunNotice.svelte";
import type { PersistedAction } from "./actionJournal.js";

const tag = {
  id: "22222222-2222-4222-8222-222222222222",
  name: "Reviewed",
  revision: 1,
  assignment_count: 0,
};
const action = { tag_id: tag.id, assign: true, total: 128 } as PersistedAction;

afterEach(cleanup);

it("names the unfinished run and confirms before discarding it", async () => {
  const onresume = vi.fn();
  const ondiscard = vi.fn();
  render(TagRunNotice, { action, tags: [tag], busy: false, onresume, ondiscard });

  expect(screen.getByText("Adding “Reviewed” to 128 documents didn't finish.")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Resume" }));
  expect(onresume).toHaveBeenCalledOnce();

  await fireEvent.click(screen.getByRole("button", { name: "Discard…" }));
  expect(ondiscard).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Discard" }));
  expect(ondiscard).toHaveBeenCalledOnce();
});

it("offers only Discard for a run it cannot read", () => {
  render(TagRunNotice, {
    action: "unreadable",
    tags: [],
    busy: false,
    onresume: vi.fn(),
    ondiscard: vi.fn(),
  });
  expect(screen.getByText("An unfinished tagging run can't be read.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Resume" })).toBeNull();
  expect(screen.getByRole("button", { name: "Discard…" })).toBeTruthy();
});
