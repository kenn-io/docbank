import * as actions from "./action-events.js";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import UploadDrawer from "./UploadDrawer.svelte";
import { APIError } from "./api-transport.js";
import * as upload from "./upload.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("upload documents drawer", () => {
  it.each(["added", "skipped"] as const)(
    "hashes and uploads selected files before refreshing the directory: %s",
    async (status) => {
      const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
      const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
      const hash = "a".repeat(64);
      vi.spyOn(upload, "hashFile").mockImplementation(async (file, _signal, onprogress) => {
        onprogress({ processed: file.size, total: file.size });
        return hash;
      });
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
              status,
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
          },
        ),
      };
      const complete = vi.fn();

      render(UploadDrawer, {
        session: "session",
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
      await fireEvent.click(screen.getByRole("button", { name: "Upload 1 file" }));

      if (status === "skipped") {
        await waitFor(() => expect(complete).toHaveBeenCalledOnce());
        expect(action).not.toHaveBeenCalled();
        expect(failure).not.toHaveBeenCalled();
        return;
      }
      expect(await screen.findByText("Added")).toBeTruthy();
      await waitFor(() => expect(complete).toHaveBeenCalledOnce());
      expect(action).toHaveBeenCalledExactlyOnceWith("session", "document_added");
      expect(failure).not.toHaveBeenCalled();
      expect(upload.hashFile).toHaveBeenCalledOnce();
      expect(channel.uploadFile).toHaveBeenCalledWith(
        3,
        file,
        hash,
        expect.any(AbortSignal),
        expect.any(Function),
      );
    },
  );

  it("refreshes and reports an unconfirmed outcome after cancellation", async () => {
    const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
    const hash = "a".repeat(64);
    vi.spyOn(upload, "hashFile").mockResolvedValue(hash);
    const channel: upload.UploadTransport = {
      uploadFile: vi.fn(
        async (
          _parentID: number,
          _file: File,
          _expectedHash: string,
          signal: AbortSignal,
          onprogress: (progress: upload.TransferProgress) => void,
        ) =>
          await new Promise<never>((_resolve, reject) => {
            onprogress({ processed: 0, total: 9 });
            signal.addEventListener(
              "abort",
              () => reject(new DOMException("The operation was aborted.", "AbortError")),
              { once: true },
            );
          }),
      ),
    };
    const complete = vi.fn();

    render(UploadDrawer, {
      session: "session",
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
    await fireEvent.click(screen.getByRole("button", { name: "Upload 1 file" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Cancel current upload" }));

    expect(await screen.findByText("Unconfirmed")).toBeTruthy();
    expect(screen.getByText(/Retry this file to check/)).toBeTruthy();
    await waitFor(() => expect(complete).toHaveBeenCalledOnce());
    expect(failure).not.toHaveBeenCalled();
  });

  it("reports two failed uploads once", async () => {
    const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
    vi.spyOn(upload, "hashFile").mockResolvedValue("a".repeat(64));
    const channel: upload.UploadTransport = {
      uploadFile: vi.fn().mockRejectedValue(new APIError("Upload failed", 422, "validation")),
    };
    render(UploadDrawer, {
      session: "session",
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
      oncomplete: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await fireEvent.change(screen.getByLabelText("Choose local files"), {
      target: { files: [new File(["a"], "a.txt"), new File(["b"], "b.txt")] },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Upload 2 files" }));
    await waitFor(() => expect(channel.uploadFile).toHaveBeenCalledTimes(2));
    expect(failure).toHaveBeenCalledExactlyOnceWith(
      "session",
      "add",
      expect.objectContaining({ status: 422, code: "validation" }),
    );
  });

  it("reports hashing cancellation without claiming an upload began", async () => {
    vi.spyOn(upload, "hashFile").mockImplementation(
      async (_file, signal) =>
        await new Promise<never>((_resolve, reject) => {
          signal.addEventListener(
            "abort",
            () => reject(new DOMException("The operation was aborted.", "AbortError")),
            { once: true },
          );
        }),
    );
    const channel: upload.UploadTransport = {
      uploadFile: vi.fn(),
    };
    const complete = vi.fn();

    render(UploadDrawer, {
      session: "session",
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
    await fireEvent.click(screen.getByRole("button", { name: "Upload 1 file" }));
    await fireEvent.click(await screen.findByRole("button", { name: "Cancel current upload" }));

    expect(await screen.findByText("Cancelled")).toBeTruthy();
    expect(screen.getByText("Cancelled before upload began.")).toBeTruthy();
    expect(channel.uploadFile).not.toHaveBeenCalled();
    expect(complete).not.toHaveBeenCalled();
  });

  it("keeps a disconnected upload visible as unconfirmed and stops the queue", async () => {
    vi.spyOn(upload, "hashFile").mockResolvedValue("a".repeat(64));
    const channel: upload.UploadTransport = {
      uploadFile: vi.fn(
        async (
          _parentID: number,
          _file: File,
          _expectedHash: string,
          _signal: AbortSignal,
          onprogress: (progress: upload.TransferProgress) => void,
        ) => {
          onprogress({ processed: 0, total: 9 });
          throw new upload.UploadChannelError("verified channel ended");
        },
      ),
    };
    const complete = vi.fn();

    render(UploadDrawer, {
      session: "session",
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
        files: [new File(["quarterly"], "quarterly.txt"), new File(["appendix"], "appendix.txt")],
      },
    });
    await fireEvent.click(screen.getByRole("button", { name: "Upload 2 files" }));

    expect(await screen.findByText("Unconfirmed")).toBeTruthy();
    expect(screen.getByText(/Run `docbank web` again, then retry this file/)).toBeTruthy();
    expect(channel.uploadFile).toHaveBeenCalledOnce();
    await waitFor(() => expect(complete).toHaveBeenCalledOnce());
  });
});

it("preserves the selected root and subfolders, reuses existing folders, and retries in place", async () => {
  vi.spyOn(upload, "hashFile").mockResolvedValue("a".repeat(64));
  const parents: number[] = [];
  let fail = true;
  const channel: upload.UploadTransport = {
    uploadFile: async (parentID, file, hash) => {
      parents.push(parentID);
      if (fail) {
        fail = false;
        throw new Error("Temporary upload failure");
      }
      return {
        status: "added",
        computed_hash: hash,
        computed_size: file.size,
        node: {
          id: 20,
          parent_id: parentID,
          name: file.name,
          kind: "file",
          size: file.size,
          revision: 1,
          created_at: "",
          modified_at: "",
        },
      };
    },
  };
  const created: { parent_id: number; name: string; kind: string }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (init?.method === "POST" && url === "/api/v1/nodes") {
      const body = JSON.parse(String(init.body));
      created.push(body);
      if (body.name === "Reports")
        return Response.json({ code: "exists", detail: "Already exists" }, { status: 409 });
      return Response.json({ id: 12, parent_id: 11, name: "2026", kind: "dir" });
    }
    if (url.startsWith("/api/v1/nodes/3/children"))
      return Response.json({ items: [{ id: 11, name: "Reports", kind: "dir" }], total: 1 });
    throw new Error(`Unexpected request ${url}`);
  });
  const complete = vi.fn();
  render(UploadDrawer, {
    session: "session",
    channel,
    directory: {
      id: 3,
      path: "/Work",
      name: "Work",
      kind: "dir",
      size: 0,
      revision: 1,
      created_at: "",
      modified_at: "",
    },
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: vi.fn(),
  });
  const first = new File(["a"], "first.txt");
  const second = new File(["b"], "second.txt");
  Object.defineProperty(first, "webkitRelativePath", { value: "Reports/2026/first.txt" });
  Object.defineProperty(second, "webkitRelativePath", { value: "Reports/2026/second.txt" });
  await fireEvent.change(screen.getByLabelText("Choose local folder"), {
    target: { files: [first, second] },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Upload 2 files" }));
  await screen.findByText("Failed");
  await waitFor(() => expect(complete).toHaveBeenCalledOnce());
  await fireEvent.click(screen.getByRole("button", { name: "Upload 1 file" }));
  await waitFor(() => expect(complete).toHaveBeenCalledTimes(2));
  expect(parents).toEqual([12, 12, 12]);
  expect(created).toEqual([
    { parent_id: 3, name: "Reports", kind: "dir" },
    { parent_id: 11, name: "2026", kind: "dir" },
  ]);
});
