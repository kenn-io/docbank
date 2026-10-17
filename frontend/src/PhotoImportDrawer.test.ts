import { afterEach, expect, it, vi } from "vite-plus/test";
import { tick } from "svelte";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { photoSelectionFiles as files } from "./photo-test-fixtures.js";
import PhotoImportDrawer from "./PhotoImportDrawer.svelte";
import { APIError } from "./api-transport.js";
import { UploadChannelError, type PhotoImportTransport, type PhotoGroupReceipt } from "./upload.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("uses the plan, uploads groups in order, reports totals, and refreshes once", async () => {
  const capture = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(new Response(null, { status: 202 }));
  const selected = files();
  const channel: PhotoImportTransport = {
    planPhotoImport: vi.fn().mockResolvedValue({
      import_id: "import-1",
      groups: [[0, 1], [2]],
      unsupported: [3],
      skip_reasons: { 3: "Unsupported type" },
    }),
    uploadPhotoGroup: vi
      .fn()
      .mockImplementation(async (_importID, _groupIndex, group, _signal, progress) => {
        progress("hashing", { processed: 0, total: 7 });
        progress("uploading", { processed: 7, total: 7 });
        return { status: group.length === 2 ? "added" : "skipped", asset_id: "photo-1" };
      }),
  };
  const complete = vi.fn();
  render(PhotoImportDrawer, {
    session: "import-session",
    channel,
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: vi.fn(),
  });
  await fireEvent.change(screen.getByLabelText("Choose photo folder"), {
    target: { files: selected },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 2 groups" }));
  await waitFor(() => expect(complete).toHaveBeenCalledOnce());
  expect(capture.mock.calls.map(([, init]) => JSON.parse(String(init?.body)))).toEqual([
    { event: "document_added", properties: { surface: "web" } },
  ]);
  expect(channel.planPhotoImport).toHaveBeenCalledWith(selected, "/");
  expect(channel.uploadPhotoGroup).toHaveBeenNthCalledWith(
    1,
    "import-1",
    0,
    selected.slice(0, 2),
    expect.any(AbortSignal),
    expect.any(Function),
  );
  expect(channel.uploadPhotoGroup).toHaveBeenNthCalledWith(
    2,
    "import-1",
    1,
    [selected[2]],
    expect.any(AbortSignal),
    expect.any(Function),
  );
  expect(screen.getByText("Added: 1")).toBeTruthy();
  expect(screen.getByText("Already in vault: 1")).toBeTruthy();
  expect(screen.getByText("Skipped: 1")).toBeTruthy();
  expect(screen.getByText("Unsupported type")).toBeTruthy();
  expect(screen.getByText("notes.txt")).toBeTruthy();
});

it("stops after the current group when cancelled", async () => {
  let finish!: (receipt: PhotoGroupReceipt) => void;
  let signal!: AbortSignal;
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "import-1", groups: [[0, 1], [2]], unsupported: [3] }),
    uploadPhotoGroup: vi.fn().mockImplementation((_importID, _destination, _group, active) => {
      signal = active;
      return new Promise((resolve) => (finish = resolve));
    }),
  };
  const complete = vi.fn();
  render(PhotoImportDrawer, {
    session: "import-session",
    channel,
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: vi.fn(),
  });
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: files() },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 2 groups" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Stop after this group" }));
  expect(signal.aborted).toBe(false);
  finish({ status: "added", asset_id: "photo-1" });
  await waitFor(() => expect(complete).toHaveBeenCalledOnce());
  expect(channel.uploadPhotoGroup).toHaveBeenCalledOnce();
  expect(screen.getByRole("button", { name: "Add 1 group" })).toBeTruthy();
});

it("reports ambiguous and failed groups and uses the chosen destination", async () => {
  const selected = [
    files()[0],
    files()[1],
    new File(["raw-a"], "movie.ARW"),
    new File(["raw-b"], "movie.CR2"),
    files()[3],
  ];
  const channel: PhotoImportTransport = {
    planPhotoImport: vi.fn().mockResolvedValue({
      import_id: "import-1",
      groups: [
        [0, 1],
        [2, 3],
      ],
      unsupported: [4],
    }),
    uploadPhotoGroup: vi
      .fn()
      .mockRejectedValueOnce(new APIError("Hash mismatch", 422, "validation"))
      .mockResolvedValueOnce({ status: "ambiguous", asset_id: "", reason: "multiple_raw" }),
  };
  const complete = vi.fn();
  render(PhotoImportDrawer, {
    session: "import-session",
    channel,
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: vi.fn(),
  });
  await fireEvent.input(screen.getByLabelText("Destination"), { target: { value: "/photos" } });
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: selected },
  });
  const destination = screen.getByLabelText<HTMLInputElement>("Destination");
  await fireEvent.click(await screen.findByRole("button", { name: "Add 2 groups" }));
  await waitFor(() => expect(complete).toHaveBeenCalledOnce());
  expect(destination.disabled).toBe(true);
  expect(screen.getByText("Multiple RAW files share this name.")).toBeTruthy();
  expect(screen.getByText("Ambiguous: 1")).toBeTruthy();
  expect(screen.getByText("Failed: 1")).toBeTruthy();
  expect(screen.getByText("Hash mismatch")).toBeTruthy();
  expect(channel.planPhotoImport).toHaveBeenCalledWith(expect.any(Array), "/photos");
  vi.mocked(channel.uploadPhotoGroup).mockResolvedValueOnce({
    status: "added",
    asset_id: "retried",
  });
  await fireEvent.click(screen.getByRole("button", { name: "Add 1 group" }));
  await waitFor(() => expect(complete).toHaveBeenCalledTimes(2));
  expect(channel.uploadPhotoGroup).toHaveBeenCalledTimes(3);
  expect(vi.mocked(channel.uploadPhotoGroup).mock.calls.map((call) => call[1])).toEqual([0, 1, 0]);
  expect(screen.queryByText("Hash mismatch")).toBeNull();
  expect(screen.getByText("Failed: 0")).toBeTruthy();
  expect(screen.getByText("Added: 1")).toBeTruthy();
  expect(channel.uploadPhotoGroup).toHaveBeenCalledWith(
    "import-1",
    0,
    expect.any(Array),
    expect.any(AbortSignal),
    expect.any(Function),
  );
});

it.each([
  new UploadChannelError("Channel ended"),
  new APIError("Destination removed", 404, "not_found"),
])("stops the queue on channel failure or an unavailable destination", async (cause) => {
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "import-1", groups: [[0, 1], [2]], unsupported: [3] }),
    uploadPhotoGroup: vi.fn().mockImplementation(async (_id, _index, group, _signal, progress) => {
      if (cause instanceof APIError) {
        const total = group.reduce((sum: number, file: File) => sum + file.size, 0);
        progress("uploading", { processed: total, total, submitted: true });
      }
      throw cause;
    }),
  };
  const authfailure = vi.fn();
  const complete = vi.fn();
  render(PhotoImportDrawer, {
    session: "import-session",
    channel,
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: authfailure,
  });
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: files() },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 2 groups" }));
  await screen.findByText(cause.message);
  expect(channel.uploadPhotoGroup).toHaveBeenCalledOnce();
  expect(complete).toHaveBeenCalledTimes(cause instanceof APIError ? 1 : 0);
  expect(authfailure).not.toHaveBeenCalled();
});

it.each([3, 0])(
  "marks a lost receipt unconfirmed and refreshes after submitting %i bytes",
  async (size) => {
    const channel: PhotoImportTransport = {
      planPhotoImport: vi
        .fn()
        .mockResolvedValue({ import_id: "import-1", groups: [[0]], unsupported: [] }),
      uploadPhotoGroup: vi
        .fn()
        .mockImplementation(async (_id, _destination, _group, _signal, progress) => {
          progress("uploading", {
            processed: size,
            total: size,
            submitted: size === 0 ? true : undefined,
          });
          throw new UploadChannelError("Channel ended");
        }),
    };
    const complete = vi.fn();
    render(PhotoImportDrawer, {
      session: "import-session",
      channel,
      onclose: vi.fn(),
      oncomplete: complete,
      onauthfailure: vi.fn(),
    });
    await fireEvent.change(screen.getByLabelText("Choose photo files"), {
      target: { files: [new File([new Uint8Array(size)], "empty.JPG")] },
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Add 1 group" }));
    await waitFor(() => expect(complete).toHaveBeenCalledOnce());
    expect(screen.getByText("Unconfirmed: 1")).toBeTruthy();
    expect(
      screen.getByText(
        "Import outcome is unconfirmed. Reload the page and choose the folder again. Identical RAW, image, and video content returns as Already in vault. Other files, including lone XMP sidecars, may be added again.",
      ),
    ).toBeTruthy();
  },
);

it.each([false, true])(
  "replans before adding after editing a destination with the drawer closed: %s",
  async (closed) => {
    const channel: PhotoImportTransport = {
      planPhotoImport: vi
        .fn()
        .mockRejectedValueOnce(new APIError("Invalid destination", 422, "invalid_name"))
        .mockResolvedValue({ import_id: "fixed", groups: [[0]], unsupported: [] }),
      uploadPhotoGroup: vi.fn(),
    };
    const view = render(PhotoImportDrawer, {
      session: "import-session",
      channel,
      onclose: vi.fn(),
      oncomplete: vi.fn(),
      onauthfailure: vi.fn(),
    });
    const selected = [files()[0]];
    await fireEvent.input(screen.getByLabelText("Destination"), { target: { value: "/bad/." } });
    await fireEvent.change(screen.getByLabelText("Choose photo files"), {
      target: { files: selected },
    });
    await screen.findByText("Invalid destination");
    expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
    await fireEvent.input(screen.getByLabelText("Destination"), { target: { value: "/photos" } });
    await fireEvent.change(screen.getByLabelText("Destination"));
    await screen.findByRole("button", { name: "Add 1 group" });
    expect(channel.planPhotoImport).toHaveBeenLastCalledWith(selected, "/photos");
    expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
    let finishPlan!: (plan: Awaited<ReturnType<PhotoImportTransport["planPhotoImport"]>>) => void;
    vi.mocked(channel.planPhotoImport).mockImplementationOnce(
      () => new Promise((resolve) => (finishPlan = resolve)),
    );
    vi.mocked(channel.uploadPhotoGroup).mockResolvedValue({ status: "added", asset_id: "photo-1" });
    await fireEvent.input(screen.getByLabelText("Destination"), { target: { value: "/trips" } });
    await fireEvent.change(screen.getByLabelText("Destination"));
    await fireEvent.blur(screen.getByLabelText("Destination"));
    expect(screen.getByRole("button", { name: "Add 1 group" }).hasAttribute("disabled")).toBe(
      false,
    );
    await fireEvent.click(screen.getByRole("button", { name: "Add 1 group" }));
    await screen.findByText("Planning");
    expect(screen.queryByRole("button", { name: /^Add \d+ groups?$/ })).toBeNull();
    expect(channel.planPhotoImport).toHaveBeenLastCalledWith(selected, "/trips");
    expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
    if (closed) await view.unmount();
    finishPlan({ import_id: "second", groups: [[0]], unsupported: [] });
    if (closed) {
      await tick();
      await tick();
      expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
      return;
    }
    await waitFor(() => expect(channel.uploadPhotoGroup).toHaveBeenCalledOnce());
    expect(channel.uploadPhotoGroup).toHaveBeenCalledWith(
      "second",
      0,
      selected,
      expect.any(AbortSignal),
      expect.any(Function),
    );
  },
);

it("cancels a group when the drawer closes before sending bytes", async () => {
  const complete = vi.fn();
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "import-1", groups: [[0]], unsupported: [] }),
    uploadPhotoGroup: vi.fn().mockImplementation(
      (_id, _index, _group, signal, progress) =>
        new Promise((_resolve, reject) => {
          progress("uploading", { processed: 0, total: 3 });
          signal.addEventListener("abort", () =>
            reject(new DOMException("Canceled", "AbortError")),
          );
        }),
    ),
  };
  const view = render(PhotoImportDrawer, {
    session: "import-session",
    channel,
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: vi.fn(),
  });
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: [files()[0]] },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 1 group" }));
  view.unmount();
  await waitFor(() =>
    expect(vi.mocked(channel.uploadPhotoGroup).mock.calls[0][3].aborted).toBe(true),
  );
  expect(complete).not.toHaveBeenCalled();
});

it("stops the queue when the channel closes after a server rejection", async () => {
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "import-1", groups: [[0, 1], [2]], unsupported: [3] }),
    uploadPhotoGroup: vi.fn(),
  };
  const view = render(PhotoImportDrawer, {
    session: "import-session",
    channel,
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  });
  vi.mocked(channel.uploadPhotoGroup).mockImplementation(async () => {
    await view.rerender({ disabledReason: "The verified upload channel ended." });
    throw new APIError("Vault storage is full", 507, "storage_full");
  });
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: files() },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 2 groups" }));
  await screen.findByText("Vault storage is full");
  expect(channel.uploadPhotoGroup).toHaveBeenCalledOnce();
  expect(screen.getByText("Failed: 1")).toBeTruthy();
});

it("shows a library refresh failure", async () => {
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "import-1", groups: [[0]], unsupported: [] }),
    uploadPhotoGroup: vi.fn().mockResolvedValue({ status: "added", asset_id: "photo-1" }),
  };
  const authfailure = vi.fn();
  render(PhotoImportDrawer, {
    session: "import-session",
    channel,
    onclose: vi.fn(),
    oncomplete: vi.fn().mockRejectedValue(new APIError("Session expired", 401, "unauthorized")),
    onauthfailure: authfailure,
  });
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: files() },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 1 group" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Session expired");
  expect(authfailure).toHaveBeenCalledOnce();
});
