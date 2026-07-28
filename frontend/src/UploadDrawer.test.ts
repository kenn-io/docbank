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
    const channel: upload.UploadTransport = {
      uploadFile: vi.fn(
      async (
        parentID: number,
        file: File,
        expectedHash: string,
        _signal: AbortSignal,
        onprogress: (progress: upload.TransferProgress) => void,
      ) => {
        onprogress({ processed: file.size, total: file.size });
        return {
          status: "added" as const,
          computed_hash: expectedHash,
          computed_size: file.size,
          node: {
            id: 8,
            parent_id: parentID,
            name: file.name,
            kind: "file" as const,
            blob_hash: expectedHash,
            size: file.size,
            revision: 1,
            created_at: "2026-07-28T00:00:00Z",
            modified_at: "2026-07-28T00:00:00Z",
          },
        };
      }),
    };
    const complete = vi.fn();

    render(UploadDrawer, {
      channel,
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
    expect(channel.uploadFile).toHaveBeenCalledWith(
      3,
      file,
      hash,
      expect.any(AbortSignal),
      expect.any(Function),
    );
  });

  it("refreshes and reports an unconfirmed outcome after cancellation", async () => {
    const hash = "a".repeat(64);
    vi.spyOn(upload, "hashFile").mockResolvedValue(hash);
    const channel: upload.UploadTransport = {
      uploadFile: vi.fn(
      async (
        _parentID: number,
        _file: File,
        _expectedHash: string,
        signal: AbortSignal,
      ) =>
        await new Promise<never>((_resolve, reject) => {
          signal.addEventListener(
            "abort",
            () =>
              reject(new DOMException("The operation was aborted.", "AbortError")),
            { once: true },
          );
        }),
      ),
    };
    const complete = vi.fn();

    render(UploadDrawer, {
      channel,
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
    await fireEvent.click(
      await screen.findByRole("button", { name: "Cancel current upload" }),
    );

    expect(await screen.findByText("Unconfirmed")).toBeTruthy();
    expect(screen.getByText(/retry this file to converge safely/)).toBeTruthy();
    await waitFor(() => expect(complete).toHaveBeenCalledOnce());
  });

  it("reports hashing cancellation without claiming an upload began", async () => {
    vi.spyOn(upload, "hashFile").mockImplementation(
      async (_file, signal) =>
        await new Promise<never>((_resolve, reject) => {
          signal.addEventListener(
            "abort",
            () =>
              reject(new DOMException("The operation was aborted.", "AbortError")),
            { once: true },
          );
        }),
    );
    const channel: upload.UploadTransport = {
      uploadFile: vi.fn(),
    };
    const complete = vi.fn();

    render(UploadDrawer, {
      channel,
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

    await fireEvent.change(screen.getByLabelText("Choose local files"), {
      target: {
        files: [new File(["quarterly"], "quarterly.txt", { type: "text/plain" })],
      },
    });
    await fireEvent.click(
      screen.getByRole("button", { name: "Upload 1 file" }),
    );
    await fireEvent.click(
      await screen.findByRole("button", { name: "Cancel current upload" }),
    );

    expect(await screen.findByText("Cancelled")).toBeTruthy();
    expect(screen.getByText("Cancelled before upload began.")).toBeTruthy();
    expect(channel.uploadFile).not.toHaveBeenCalled();
    expect(complete).not.toHaveBeenCalled();
  });
});
