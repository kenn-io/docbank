import { afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoDetails from "./PhotoDetails.svelte";
import { photo } from "./photo-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("copies the displayed photo version and IDs, including after moving to another photo", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/audit/status")) return Response.json({ membership: { protected: false } });
    const id = Number(url.split("/").at(-1));
    return Response.json({ id, current_version_id: photo(id).content_version_id });
  });
  const first = photo(1);
  const view = render(PhotoDetails, { photo: first, session: "synthetic", onauthfailure: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Copy Version" }));
  expect(writeText).toHaveBeenLastCalledWith(first.content_version_id);
  await fireEvent.click(screen.getByRole("button", { name: "Copy ID" }));
  expect(writeText).toHaveBeenLastCalledWith("1");
  const second = photo(2);
  await view.rerender({ photo: second, session: "synthetic", onauthfailure: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Copy File ID" }));
  expect(writeText).toHaveBeenLastCalledWith(second.display_file_id);
  expect(screen.getByRole("heading", { name: "Photo 2.jpg" })).toBeTruthy();
});

it("copies the current file path and hash and discards a previous photo's late details", async () => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  let release!: (response: Response) => void;
  const first = photo(1);
  const second = photo(2);
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/nodes/1") return new Promise<Response>((resolve) => (release = resolve));
    if (url === "/api/v1/nodes/2")
      return Response.json({
        id: 2,
        path: "/photos/lake.jpg",
        current_version_id: second.content_version_id,
        blob_hash: "b".repeat(64),
        size: 64,
        modified_at: "2026-01-01T00:00:00Z",
      });
    if (url.startsWith("/api/v1/audit/status?"))
      return Response.json({ membership: { protected: false } });
    throw new Error(`Unexpected request ${url}`);
  });
  const view = render(PhotoDetails, { photo: first, session: "synthetic", onauthfailure: vi.fn() });
  await view.rerender({ photo: second, session: "synthetic", onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Copy Path" }));
  expect(writeText).toHaveBeenLastCalledWith("/photos/lake.jpg");
  await fireEvent.click(screen.getByRole("button", { name: "Copy SHA-256" }));
  expect(writeText).toHaveBeenLastCalledWith("b".repeat(64));
  await act(() =>
    release(
      Response.json({
        id: 1,
        path: "/photos/old.jpg",
        current_version_id: first.content_version_id,
      }),
    ),
  );
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Copy Path" }).textContent).toContain(
      "/photos/lake.jpg",
    ),
  );
});

it("keeps a replaced file's details separate from the displayed photo version", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
    Response.json(
      String(input).includes("/audit/status")
        ? { membership: { protected: false } }
        : {
            id: 1,
            path: "/photos/replaced.jpg",
            current_version_id: "replacement-version",
            blob_hash: "c".repeat(64),
            size: 128,
          },
    ),
  );
  render(PhotoDetails, { photo: photo(1), session: "synthetic", onauthfailure: vi.fn() });
  expect(
    await screen.findByText("This file changed. Reopen the photo to refresh its details."),
  ).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Copy SHA-256" })).toBeNull();
  expect(screen.getByRole("button", { name: "Copy Version" }).textContent).toContain(
    photo(1).content_version_id,
  );
});
