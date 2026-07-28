import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/svelte";
import UploadDrawer from "./UploadDrawer.svelte";
import * as upload from "./upload.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("upload documents drawer", () => {
  it("hashes and uploads selected files before refreshing the directory", async () => {
    const hash = "a".repeat(64);
    vi.spyOn(upload, "hashFile").mockImplementation(
      async (file, _signal, onprogress) => {
        onprogress({ processed: file.size, total: file.size });
        return hash;
      },
    );
    vi.spyOn(upload, "uploadFile").mockImplementation(
      async (_session, parentID, file, expectedHash, _signal, onprogress) => {
        onprogress({ processed: file.size, total: file.size });
        return {
          status: "added",
          computed_hash: expectedHash,
          computed_size: file.size,
          node: {
            id: 8,
            parent_id: parentID,
            name: file.name,
            kind: "file",
            blob_hash: expectedHash,
            size: file.size,
            revision: 1,
            created_at: "2026-07-28T00:00:00Z",
            modified_at: "2026-07-28T00:00:00Z",
          },
        };
      },
    );
    const complete = vi.fn();

    render(UploadDrawer, {
      session: "short-lived",
      directory: {
        id: 3,
        name: "Reports",
        path: "/Reports",
        kind: "dir",
        size: 0,
        revision: 1,
        created_at: "2026-07-28T00:00:00Z",
        modified_at: "2026-07-28T00:00:00Z",
      },
      onclose: vi.fn(),
      oncomplete: complete,
      onauthfailure: vi.fn(),
    });

    const file = new File(["quarterly"], "quarterly.txt", {
      type: "text/plain",
    });
    await fireEvent.change(screen.getByLabelText("Choose local files"), {
      target: { files: [file] },
    });
    await fireEvent.click(
      screen.getByRole("button", { name: "Upload 1 file" }),
    );

    expect(await screen.findByText("Added")).toBeTruthy();
    expect(screen.getByText(/Created node 8, revision 1/)).toBeTruthy();
    await waitFor(() => expect(complete).toHaveBeenCalledOnce());
    expect(upload.hashFile).toHaveBeenCalledOnce();
    expect(upload.uploadFile).toHaveBeenCalledWith(
      "short-lived",
      3,
      file,
      hash,
      expect.any(AbortSignal),
      expect.any(Function),
    );
  });
});
