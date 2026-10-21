import * as actions from "./action-events.js";
import { Blob as NodeBlob, File as NodeFile } from "node:buffer";
import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import { tick } from "svelte";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { photoSelectionFiles as files } from "./photo-test-fixtures.js";
import PhotoImportDrawer from "./PhotoImportDrawer.svelte";
import { APIError } from "./api-transport.js";
import {
  UploadChannelError,
  type PhotoImportTransport,
  type PhotoGroupReceipt,
  type TransferProgress,
} from "./upload.js";

const declarations = vi.hoisted(() => vi.fn());
vi.mock("./upload.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./upload.js")>();
  return {
    ...actual,
    photoImportDeclaration: (...args: Parameters<typeof actual.photoImportDeclaration>) => {
      declarations();
      return actual.photoImportDeclaration(...args);
    },
  };
});

const savedImports = vi.hoisted(
  () => new Map<string, import("./photoImportStorage.js").PhotoImportCheckpoint>(),
);
vi.mock("./photoImportStorage.js", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./photoImportStorage.js")>()),
  photoImportStorage: (vaultID: string) => ({
    async saved() {
      return savedImports.get(vaultID);
    },
    async load() {
      return savedImports.get(vaultID);
    },
    async replace(
      next: import("./photoImportStorage.js").PhotoImportCheckpoint | undefined,
      expected?: import("./photoImportStorage.js").PhotoImportCheckpoint,
    ) {
      const current = savedImports.get(vaultID);
      if (current?.revision !== expected?.revision || current?.sourceID !== expected?.sourceID)
        throw new APIError(
          "Import progress changed in another tab. Close and reopen Add photos.",
          409,
          "checkpoint_changed",
        );
      if (next) savedImports.set(vaultID, next);
      else savedImports.delete(vaultID);
    },
  }),
}));

beforeEach(() => {
  savedImports.clear();
  vi.stubGlobal("File", NodeFile);
});

it.each(["conflict", "auth", "abort", "after item failure"])(
  "reports outer Add failures once with exclusions: %s",
  async (outcome) => {
    const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
    const module = await import("./photoImportStorage.js");
    const createStorage = module.photoImportStorage;
    let rejectSaved = false;
    vi.spyOn(module, "photoImportStorage").mockImplementation((id, session) => {
      const storage = createStorage(id, session);
      return {
        ...storage,
        async saved() {
          if (rejectSaved)
            throw outcome === "auth"
              ? new APIError("Session expired", 401, "unauthorized")
              : new DOMException("Canceled", "AbortError");
          return storage.saved();
        },
      };
    });
    const channel: PhotoImportTransport = {
      planPhotoImport: vi
        .fn()
        .mockResolvedValue({ import_id: "outer", groups: [[0], [1]], unsupported: [] }),
      uploadPhotoGroup: vi.fn().mockRejectedValue(new APIError("Upload failed", 500, "internal")),
    };
    const auth = vi.fn();
    render(PhotoImportDrawer, {
      channel,
      vaultID: "outer-failure",
      session: "session",
      onclose: vi.fn(),
      oncomplete: vi.fn(),
      onauthfailure: auth,
    });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" })).toHaveProperty(
        "disabled",
        false,
      ),
    );
    await fireEvent.change(screen.getByLabelText("Choose photo files"), {
      target: { files: files().slice(0, 2) },
    });
    const add = await screen.findByRole("button", { name: "Add 2 groups" });
    const storage = createStorage("outer-failure", "session");
    if (outcome === "conflict" || outcome === "after item failure") {
      const checkpoint = await storage.load();
      const change = () => storage.replace({ ...checkpoint!, revision: "other-tab" }, checkpoint);
      if (outcome === "conflict") await change();
      else
        vi.mocked(channel.uploadPhotoGroup).mockImplementationOnce(async () => {
          await change();
          throw new APIError("Upload failed", 500, "internal");
        });
    } else {
      rejectSaved = true;
    }
    await fireEvent.click(add);
    if (outcome === "auth") await waitFor(() => expect(auth).toHaveBeenCalledOnce());
    else
      await screen.findByText(
        outcome === "abort"
          ? /Canceled/
          : "Import progress changed in another tab. Close and reopen Add photos.",
      );
    if (outcome === "auth" || outcome === "abort") expect(failure).not.toHaveBeenCalled();
    else expect(failure).toHaveBeenCalledExactlyOnceWith("session", "add");
  },
);

afterEach(() => {
  vi.unstubAllGlobals();
  cleanup();
  vi.restoreAllMocks();
});

it("keeps restart resume after checkpoint bootstrap retries on a new profile", async () => {
  const module = await import("./photoImportStorage.js");
  const actual = await vi.importActual<typeof module>("./photoImportStorage.js");
  vi.spyOn(module, "photoImportStorage").mockImplementation(actual.photoImportStorage);
  let saved: import("./photoImportStorage.js").PhotoImportCheckpoint | undefined;
  let headers = 0;
  const fetch = vi.fn(async (url: string, options: RequestInit) => {
    if (url.endsWith("/header")) {
      headers++;
      return Response.json({ ready: headers > 1 });
    }
    if (options.method === "PUT") {
      saved = JSON.parse(options.body as string).next;
      return new Response(null, { status: 204 });
    }
    return Response.json({ checkpoint: saved });
  });
  vi.stubGlobal("fetch", fetch);
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "fresh", groups: [[0]], unsupported: [] }),
    uploadPhotoGroup: vi.fn(),
  };
  render(PhotoImportDrawer, {
    channel,
    vaultID: "fresh-profile",
    session: "session",
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" })).toHaveProperty("disabled", false),
  );
  expect(screen.queryByText(/Resume is unavailable/)).toBeNull();
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: [files()[0]] },
  });
  await screen.findByRole("button", { name: "Add 1 group" });
  expect(saved?.sourceID).toEqual(expect.any(String));
  expect(screen.getByRole("button", { name: "Resume import" })).toHaveProperty("disabled", false);
  expect(screen.queryByText(/Resume is unavailable/)).toBeNull();
});

it.each(["open", "closed", "competing"] as const)(
  "clears an empty plan without replacing newer progress when the drawer is %s",
  async (state) => {
    const module = await import("./photoImportStorage.js");
    const createStorage = module.photoImportStorage;
    const storage = createStorage("unsupported-vault", "session");
    const original = {
      sourceID: "previous-source",
      revision: "previous-revision",
      destination: "/photos",
      manifestVersion: 2,
      manifestHash: "a".repeat(64),
      pickerKind: "files" as const,
    };
    await storage.replace(original);
    let removal: Promise<void> | undefined;
    vi.spyOn(module, "photoImportStorage").mockImplementation((id) => {
      const current = createStorage(id, "session");
      return {
        ...current,
        replace(next, expected) {
          const result = current.replace(next, expected);
          if (!next) removal = result;
          return result;
        },
      };
    });
    let finishPlan!: (plan: Awaited<ReturnType<PhotoImportTransport["planPhotoImport"]>>) => void;
    const channel: PhotoImportTransport = {
      planPhotoImport: vi
        .fn<PhotoImportTransport["planPhotoImport"]>()
        .mockImplementationOnce(() => new Promise((resolve) => (finishPlan = resolve)))
        .mockResolvedValueOnce({ import_id: "supported", groups: [[0]], unsupported: [] }),
      uploadPhotoGroup: vi.fn(),
    };
    const view = render(PhotoImportDrawer, {
      channel,
      vaultID: "unsupported-vault",
      session: "session",
      onclose: vi.fn(),
      oncomplete: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
        false,
      ),
    );
    await fireEvent.change(screen.getByLabelText("Choose photo files"), {
      target: { files: [files()[3]] },
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Replace import" }));
    await waitFor(() => expect(finishPlan).toBeDefined());

    if (state !== "open") await view.unmount();
    const newer = { ...original, sourceID: "other-source", revision: "other-revision" };
    if (state === "competing") await storage.replace(newer, original);
    finishPlan({
      import_id: "unsupported",
      groups: [],
      unsupported: [0],
      skip_reasons: { 0: "Unsupported type" },
    });
    if (state === "competing") {
      await waitFor(() => expect(removal).toBeDefined());
      await expect(removal).rejects.toThrow("Import progress changed in another tab");
      expect(await storage.load()).toEqual(newer);
    } else if (state === "closed") {
      await waitFor(async () => expect(await storage.saved()).toBeUndefined());
    } else {
      await screen.findByText("Unsupported type");
      expect(await storage.saved()).toBeUndefined();
      await fireEvent.change(screen.getByLabelText("Choose photo files"), {
        target: { files: [files()[0]] },
      });
      await screen.findByRole("button", { name: "Add 1 group" });
      expect(screen.queryByRole("dialog", { name: "Replace the unfinished import?" })).toBeNull();
      expect(channel.planPhotoImport).toHaveBeenCalledTimes(2);
    }
    if (state !== "open") expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
  },
);

it.each([false, true])(
  "uses the plan, uploads groups in order, reports totals, and refreshes once: skipped=%s",
  async (skipped) => {
    const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
    const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
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
        .mockImplementation(async (_importID, groupIndex, _group, _signal, progress) => {
          progress("hashing", { processed: 0, total: 7 });
          progress("uploading", { processed: 7, total: 7 });
          return {
            status: skipped || groupIndex === 1 ? "skipped" : "added",
            asset_id: "photo-1",
          };
        }),
    };
    const complete = vi.fn();
    render(PhotoImportDrawer, {
      vaultID: "vault-test",
      session: "session",
      channel,
      onclose: vi.fn(),
      oncomplete: complete,
      onauthfailure: vi.fn(),
    });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
        false,
      ),
    );
    await fireEvent.change(screen.getByLabelText("Choose photo folder"), {
      target: { files: selected },
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Add 2 groups" }));
    await waitFor(() => expect(complete).toHaveBeenCalledOnce());
    expect(failure).not.toHaveBeenCalled();
    if (skipped) {
      expect(action).not.toHaveBeenCalled();
      return;
    }
    expect(action).toHaveBeenCalledExactlyOnceWith("session", "document_added");
    expect(channel.planPhotoImport).toHaveBeenCalledWith(
      selected,
      "/",
      expect.any(String),
      expect.any(AbortSignal),
      expect.any(Function),
    );
    expect(channel.uploadPhotoGroup).toHaveBeenNthCalledWith(
      1,
      "import-1",
      0,
      selected.slice(0, 2),
      expect.any(AbortSignal),
      expect.any(Function),
      ["", ""],
    );
    expect(channel.uploadPhotoGroup).toHaveBeenNthCalledWith(
      2,
      "import-1",
      1,
      [selected[2]],
      expect.any(AbortSignal),
      expect.any(Function),
      [""],
    );
    expect(screen.getByText("Added: 1")).toBeTruthy();
    expect(screen.getByText("Already in vault: 1")).toBeTruthy();
    expect(screen.getByText("Skipped: 1")).toBeTruthy();
    expect(screen.getByText("Unsupported type")).toBeTruthy();
    expect(screen.getByText("notes.txt")).toBeTruthy();
  },
);

it("starts a new shoot's first group after reading only that group's files", async () => {
  const selected = files();
  const reads = selected.map((file) => vi.spyOn(file, "slice"));
  const readCounts = () => reads.map((read) => read.mock.calls.length);
  let readsAtUpload: number[] | undefined;
  let hashesAtUpload: string[] | undefined;
  const channel: PhotoImportTransport = {
    planPhotoImport: vi.fn().mockResolvedValue({
      import_id: "new-shoot",
      groups: [[0, 1], [2]],
      unsupported: [3],
      skip_reasons: { 3: "Unsupported type" },
    }),
    uploadPhotoGroup: vi
      .fn()
      .mockImplementation(async (_id, _group, _files, _signal, _p, hashes) => {
        readsAtUpload ??= readCounts();
        hashesAtUpload ??= [...hashes];
        return { status: "added", asset_id: "photo-1" };
      }),
  };
  render(PhotoImportDrawer, {
    vaultID: "vault-new-shoot",
    session: "session",
    channel,
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  await fireEvent.change(screen.getByLabelText("Choose photo folder"), {
    target: { files: selected },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 2 groups" }));
  await waitFor(() => expect(channel.uploadPhotoGroup).toHaveBeenCalledTimes(2));
  expect(readsAtUpload).toEqual([0, 0, 0, 0]);
  expect(hashesAtUpload).toEqual(["", ""]);
});

it("shows Hashing only while the files the server asked for are read", async () => {
  const selected = [files()[0]];
  let report!: (progress: TransferProgress) => void;
  let finish!: (plan: Awaited<ReturnType<PhotoImportTransport["planPhotoImport"]>>) => void;
  const channel: PhotoImportTransport = {
    planPhotoImport: vi.fn().mockImplementation(
      (_files, _destination, _source, _signal, onprogress) =>
        new Promise((resolve) => {
          report = onprogress;
          finish = resolve;
        }),
    ),
    uploadPhotoGroup: vi.fn().mockResolvedValue({ status: "added", asset_id: "photo-1" }),
  };
  render(PhotoImportDrawer, {
    vaultID: "vault-hashing",
    session: "session",
    channel,
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: selected },
  });
  await screen.findByText("Planning");
  report({ processed: 1, total: 3 });
  await screen.findByText("Hashing");
  expect(screen.getByRole("progressbar", { name: "Hashing" })).toHaveProperty("value", 1);
  finish({
    import_id: "continued",
    groups: [[0]],
    unsupported: [],
    hashes: new Map([[selected[0], "a".repeat(64)]]),
  });
  await screen.findByRole("button", { name: "Add 1 group" });
  expect(screen.queryByText("Hashing")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Add 1 group" }));
  await waitFor(() => expect(channel.uploadPhotoGroup).toHaveBeenCalledOnce());
  expect(vi.mocked(channel.uploadPhotoGroup).mock.calls[0]![5]).toEqual(["a".repeat(64)]);
});

it("checks the declaration limits once per selection", async () => {
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "once", groups: [[0]], unsupported: [] }),
    uploadPhotoGroup: vi.fn(),
  };
  render(PhotoImportDrawer, {
    vaultID: "vault-once",
    session: "session",
    channel,
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  declarations.mockClear();
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: [files()[0]] },
  });
  await screen.findByRole("button", { name: "Add 1 group" });
  expect(declarations).toHaveBeenCalledOnce();
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
    vaultID: "vault-test",
    session: "session",
    channel,
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: vi.fn(),
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: files() },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 2 groups" }));
  await waitFor(() => expect(channel.uploadPhotoGroup).toHaveBeenCalledOnce());
  await fireEvent.click(await screen.findByRole("button", { name: "Stop after this group" }));
  await waitFor(() => expect(signal).toBeDefined());
  expect(signal.aborted).toBe(false);
  finish({ status: "added", asset_id: "photo-1" });
  await waitFor(() => expect(complete).toHaveBeenCalledOnce());
  expect(channel.uploadPhotoGroup).toHaveBeenCalledOnce();
  expect(screen.getByRole("button", { name: "Add 1 group" })).toBeTruthy();
});

it("reports ambiguous and failed groups and uses the chosen destination", async () => {
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
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
    vaultID: "vault-test",
    session: "session",
    channel,
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: vi.fn(),
  });
  await fireEvent.input(screen.getByLabelText("Destination"), { target: { value: "/photos" } });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
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
  expect(failure).toHaveBeenCalledExactlyOnceWith("session", "add");
  expect(channel.planPhotoImport).toHaveBeenCalledWith(
    expect.any(Array),
    "/photos",
    expect.any(String),
    expect.any(AbortSignal),
    expect.any(Function),
  );
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
    ["", ""],
  );
});

it.each([
  {
    name: "channel failure",
    cause: new UploadChannelError("Channel ended"),
    submitted: false,
    disabledReason: undefined,
    completed: 0,
  },
  {
    name: "removed destination",
    cause: new APIError("Destination removed", 404, "not_found"),
    submitted: true,
    disabledReason: undefined,
    completed: 1,
  },
  {
    name: "storage full after channel loss",
    cause: new APIError("Vault storage is full", 507, "storage_full"),
    submitted: false,
    disabledReason: "The verified upload channel ended.",
    completed: 0,
  },
])("stops the queue on $name", async ({ cause, submitted, disabledReason, completed }) => {
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "import-1", groups: [[0, 1], [2]], unsupported: [3] }),
    uploadPhotoGroup: vi.fn(),
  };
  const authfailure = vi.fn();
  const complete = vi.fn();
  let view: ReturnType<typeof render>;
  channel.uploadPhotoGroup = vi
    .fn()
    .mockImplementation(async (_id, _index, group, _signal, progress) => {
      if (submitted) {
        const total = group.reduce((sum: number, file: File) => sum + file.size, 0);
        progress("uploading", { processed: total, total, submitted: true });
      }
      if (disabledReason) await view.rerender({ disabledReason });
      throw cause;
    });
  view = render(PhotoImportDrawer, {
    vaultID: "vault-test",
    session: "session",
    channel,
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: authfailure,
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: files() },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 2 groups" }));
  await screen.findByText(cause.message);
  expect(channel.uploadPhotoGroup).toHaveBeenCalledOnce();
  expect(complete).toHaveBeenCalledTimes(completed);
  expect(authfailure).not.toHaveBeenCalled();
  if (cause instanceof APIError && cause.status === 507)
    expect(screen.getByText("Failed: 1")).toBeTruthy();
});

it.each([
  { size: 3, sessionOnly: false },
  { size: 0, sessionOnly: false },
  { size: 3, sessionOnly: true },
])(
  "marks a lost receipt unconfirmed and refreshes after submitting $size bytes with sessionOnly=$sessionOnly",
  async ({ size, sessionOnly }) => {
    if (sessionOnly) {
      const module = await import("./photoImportStorage.js");
      const createStorage = module.photoImportStorage;
      vi.spyOn(module, "photoImportStorage").mockImplementation((id, session) => ({
        ...createStorage(id, session),
        load: async () => {
          throw new APIError("Storage unavailable", 500, "checkpoint_storage");
        },
      }));
    }
    const channel: PhotoImportTransport = {
      planPhotoImport: vi
        .fn()
        .mockResolvedValue({ import_id: "import-1", groups: [[0]], unsupported: [] }),
      uploadPhotoGroup: vi.fn(),
    };
    const complete = vi.fn();
    let view: ReturnType<typeof render> | undefined;
    channel.uploadPhotoGroup = vi.fn(async (_id, _destination, _group, _signal, progress) => {
      progress("uploading", {
        processed: size,
        total: size,
        submitted: size === 0 ? true : undefined,
      });
      await view?.rerender({ disabledReason: "The verified upload channel ended." });
      throw new UploadChannelError("Channel ended");
    });
    view = render(PhotoImportDrawer, {
      vaultID: "vault-test",
      session: "session",
      channel,
      onclose: vi.fn(),
      oncomplete: complete,
      onauthfailure: vi.fn(),
    });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
        false,
      ),
    );
    await fireEvent.change(screen.getByLabelText("Choose photo files"), {
      target: { files: [new File([new Uint8Array(size)], "empty.JPG")] },
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Add 1 group" }));
    await waitFor(() => expect(complete).toHaveBeenCalledOnce());
    expect(screen.getByText("Unconfirmed: 1")).toBeTruthy();
    expect(screen.queryByText("Keep this tab open until the import finishes.")).toBeNull();
    expect(screen.getByText("The verified upload channel ended.")).toBeTruthy();
    const guidance = screen.getByText(/Import outcome is unconfirmed\./).textContent;
    if (sessionOnly) {
      expect(guidance).toContain(
        "Run docbank web again, then choose the same files or folder again.",
      );
      expect(guidance).toContain("Photos and videos already in the vault are skipped.");
      expect(guidance).toContain(
        "Other files, including standalone XMP sidecars, may be added again.",
      );
      expect(guidance).not.toMatch(/resume/i);
    } else {
      expect(guidance).toBe(
        "Import outcome is unconfirmed. Reload the page, then choose Resume import and reselect the same files or folder.",
      );
    }
  },
);

it.each([false, true])(
  "replans after editing a destination (closed during plan: %s)",
  async (closed) => {
    const channel: PhotoImportTransport = {
      planPhotoImport: vi
        .fn()
        .mockRejectedValueOnce(new APIError("Invalid destination", 422, "invalid_name"))
        .mockResolvedValue({ import_id: "fixed", groups: [[0]], unsupported: [] }),
      uploadPhotoGroup: vi.fn(),
    };
    const view = render(PhotoImportDrawer, {
      vaultID: "vault-test",
      session: "session",
      channel,
      onclose: vi.fn(),
      oncomplete: vi.fn(),
      onauthfailure: vi.fn(),
    });
    const selected = [files()[0]];
    await fireEvent.input(screen.getByLabelText("Destination"), { target: { value: "/bad/." } });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
        false,
      ),
    );
    await fireEvent.change(screen.getByLabelText("Choose photo files"), {
      target: { files: selected },
    });
    await screen.findByText("Invalid destination");
    expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
    await fireEvent.input(screen.getByLabelText("Destination"), { target: { value: "/photos" } });
    await fireEvent.change(screen.getByLabelText("Destination"));
    await screen.findByRole("button", { name: "Add 1 group" });
    expect(channel.planPhotoImport).toHaveBeenLastCalledWith(
      selected,
      "/photos",
      expect.any(String),
      expect.any(AbortSignal),
      expect.any(Function),
    );
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
    await waitFor(() =>
      expect(channel.planPhotoImport).toHaveBeenLastCalledWith(
        selected,
        "/trips",
        expect.any(String),
        expect.any(AbortSignal),
        expect.any(Function),
      ),
    );
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
      [""],
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
    vaultID: "vault-test",
    session: "session",
    channel,
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: vi.fn(),
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
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

it("shows a library refresh failure", async () => {
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "import-1", groups: [[0]], unsupported: [] }),
    uploadPhotoGroup: vi.fn().mockResolvedValue({ status: "added", asset_id: "photo-1" }),
  };
  const authfailure = vi.fn();
  render(PhotoImportDrawer, {
    vaultID: "vault-test",
    session: "session",
    channel,
    onclose: vi.fn(),
    oncomplete: vi.fn().mockRejectedValue(new APIError("Session expired", 401, "unauthorized")),
    onauthfailure: authfailure,
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: files() },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 1 group" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Session expired");
  expect(authfailure).toHaveBeenCalledOnce();
});

it("resumes an interrupted folder with its original source on a fresh channel", async () => {
  const selected = files();
  for (const file of selected)
    Object.defineProperty(file, "webkitRelativePath", { value: `Trip/${file.name}` });
  const sources: string[] = [];
  const channel = {
    planPhotoImport: vi.fn(async (_files: File[], _destination: string, sourceID: string) => {
      sources.push(sourceID);
      return { import_id: `plan-${sources.length}`, groups: [[0], [1], [2]], unsupported: [3] };
    }),
    uploadPhotoGroup: vi.fn(
      async (
        _plan: string,
        group: number,
        _files: File[],
        _signal: AbortSignal,
        progress: Function,
      ) => {
        progress("uploading", { processed: 1, total: 1 });
        if (group === 1 && sources.length === 1) throw new UploadChannelError("Disconnected");
        return { status: "added" as const, asset_id: `asset-${group}` };
      },
    ),
  };
  const props = {
    channel,
    vaultID: "vault-resume",
    session: "session",
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  };
  render(PhotoImportDrawer, props);
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  await fireEvent.change(screen.getByLabelText("Choose photo folder"), {
    target: { files: selected },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 3 groups" }));
  await screen.findByText("Unconfirmed: 1");
  cleanup();
  const freshChannel = {
    ...channel,
    planPhotoImport: vi.fn(channel.planPhotoImport),
    uploadPhotoGroup: vi.fn(channel.uploadPhotoGroup),
  };
  render(PhotoImportDrawer, { ...props, channel: freshChannel });
  const folderPicker = vi.spyOn(
    screen.getByLabelText<HTMLInputElement>("Choose photo folder"),
    "click",
  );
  await fireEvent.click(await screen.findByRole("button", { name: "Resume import" }));
  expect(folderPicker).toHaveBeenCalledOnce();
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  await fireEvent.change(screen.getByLabelText("Choose photo folder"), {
    target: { files: selected },
  });
  await fireEvent.click(await screen.findByRole("button", { name: "Add 3 groups" }));
  await screen.findByText("Added: 3");
  expect(sources[0]).toBeTruthy();
  expect(sources[1]).toBe(sources[0]);
  cleanup();
  render(PhotoImportDrawer, props);
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  await fireEvent.change(screen.getByLabelText("Choose photo folder"), {
    target: { files: selected },
  });
  await screen.findByRole("button", { name: "Add 3 groups" });
  expect(sources[2]).not.toBe(sources[0]);
});

it.each(["content", "destination"])(
  "refuses a resumed folder with changed %s and keeps the checkpoint",
  async (change) => {
    const { photoImportStorage } = await import("./photoImportStorage.js");
    const { photoImportManifest } = await import("./photoImportStorage.js");
    const selected = [files()[0]];
    const checkpoint = {
      sourceID: "source-a",
      revision: "first",
      destination: "/photos",
      manifestVersion: 2,
      manifestHash: photoImportManifest(selected).digest,
      pickerKind: "folder" as const,
    };
    const storage = photoImportStorage("vault-mismatch", "session");
    await storage.replace(checkpoint);
    const channel: PhotoImportTransport = { planPhotoImport: vi.fn(), uploadPhotoGroup: vi.fn() };
    render(PhotoImportDrawer, {
      channel,
      vaultID: "vault-mismatch",
      session: "session",
      onclose: vi.fn(),
      oncomplete: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await fireEvent.click(await screen.findByRole("button", { name: "Resume import" }));
    if (change === "destination")
      await fireEvent.input(screen.getByLabelText("Destination"), { target: { value: "/other" } });
    const changed =
      change === "content"
        ? new File(["bad!"], selected[0].name, { lastModified: selected[0].lastModified })
        : selected[0];
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
        false,
      ),
    );
    await fireEvent.change(screen.getByLabelText("Choose photo folder"), {
      target: { files: [changed] },
    });
    await screen.findByText(/These files or destination differ/);
    expect(channel.planPhotoImport).not.toHaveBeenCalled();
    expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
    expect(await storage.load()).toEqual(checkpoint);
  },
);

it.each([
  {
    name: "storage fails on load",
    loadError: new APIError("Storage unavailable", 500, "checkpoint_storage"),
    replaceError: new APIError("Storage unavailable", 500, "checkpoint_storage"),
    allowImport: true,
  },
  {
    name: "storage fails on replace",
    loadError: undefined,
    replaceError: new APIError("Storage unavailable", 500, "checkpoint_storage"),
    allowImport: true,
  },
  {
    name: "profile cookie is missing",
    loadError: new APIError("Allow cookies", 409, "checkpoint_cookie"),
    replaceError: new APIError("Allow cookies", 409, "checkpoint_cookie"),
    allowImport: true,
  },
  {
    name: "load is unauthorized",
    loadError: new APIError("unauthorized", 401, "unauthorized"),
    replaceError: undefined,
    allowImport: false,
  },
  {
    name: "load has an invalid format",
    loadError: new APIError("checkpoint_format", 409, "checkpoint_format"),
    replaceError: undefined,
    allowImport: false,
  },
  {
    name: "replace is unauthorized",
    loadError: undefined,
    replaceError: new APIError("unauthorized", 401, "unauthorized"),
    allowImport: false,
  },
  {
    name: "replace has an invalid format",
    loadError: undefined,
    replaceError: new APIError("checkpoint_format", 409, "checkpoint_format"),
    allowImport: false,
  },
])("handles checkpoint failure when $name", async ({ loadError, replaceError, allowImport }) => {
  const module = await import("./photoImportStorage.js");
  const createStorage = module.photoImportStorage;
  vi.spyOn(module, "photoImportStorage").mockImplementation((id, session) => ({
    ...createStorage(id, session),
    ...(loadError ? { load: async () => Promise.reject(loadError) } : {}),
    ...(replaceError ? { replace: async () => Promise.reject(replaceError) } : {}),
  }));
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "checkpoint-case", groups: [[0]], unsupported: [] }),
    uploadPhotoGroup: vi.fn().mockResolvedValue({ status: "added", asset_id: "photo-1" }),
  };
  const auth = vi.fn();
  const complete = vi.fn();
  render(PhotoImportDrawer, {
    channel,
    vaultID: "checkpoint-case",
    session: "session",
    onclose: vi.fn(),
    oncomplete: complete,
    onauthfailure: auth,
  });
  if (loadError && !allowImport) {
    await screen.findByText(loadError.message);
    expect(channel.planPhotoImport).not.toHaveBeenCalled();
    expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
    expect(auth).toHaveBeenCalledTimes(loadError.status === 401 ? 1 : 0);
    return;
  }
  if (loadError) await screen.findByText(/Resume is unavailable/);
  else
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" })).toHaveProperty(
        "disabled",
        false,
      ),
    );
  const selected = [files()[0]];
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: selected },
  });
  if (!allowImport) {
    await screen.findByText((replaceError ?? loadError)!.message);
    expect(channel.planPhotoImport).toHaveBeenCalledOnce();
    expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
    expect(auth).toHaveBeenCalledTimes(replaceError?.status === 401 ? 1 : 0);
    return;
  }
  await waitFor(() =>
    expect(channel.planPhotoImport).toHaveBeenCalledWith(
      selected,
      "/",
      expect.any(String),
      expect.any(AbortSignal),
      expect.any(Function),
    ),
  );
  await screen.findByText(/Resume is unavailable/);
  await fireEvent.click(await screen.findByRole("button", { name: "Add 1 group" }));
  await waitFor(() => expect(complete).toHaveBeenCalledOnce());
  expect(channel.uploadPhotoGroup).toHaveBeenCalledWith(
    "checkpoint-case",
    0,
    selected,
    expect.any(AbortSignal),
    expect.any(Function),
    expect.any(Array),
  );
  expect(auth).not.toHaveBeenCalled();
  expect(savedImports.has("checkpoint-case")).toBe(false);
});

it("rejects too many files before reading them or replacing saved progress", async () => {
  const selected = Array.from({ length: 32769 }, (_, index) => new File(["photo"], `${index}.jpg`));
  const reads = vi.spyOn(NodeBlob.prototype, "arrayBuffer");
  const module = await import("./photoImportStorage.js");
  const createStorage = module.photoImportStorage;
  const checkpoint = {
    sourceID: "20000000-0000-4000-8000-000000000001",
    revision: "30000000-0000-4000-8000-000000000001",
    destination: "/photos",
    manifestVersion: 2,
    manifestHash: "a".repeat(64),
    pickerKind: "folder" as const,
  };
  const storage = createStorage("vault-limit", "session");
  await storage.replace(checkpoint);
  const replace = vi.fn(storage.replace);
  vi.spyOn(module, "photoImportStorage").mockImplementation((id, session) => ({
    ...createStorage(id, session),
    replace,
  }));
  const channel: PhotoImportTransport = {
    planPhotoImport: vi.fn(),
    uploadPhotoGroup: vi.fn(),
  };
  render(PhotoImportDrawer, {
    channel,
    vaultID: "vault-limit",
    session: "session",
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" })).toHaveProperty("disabled", false),
  );
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: selected },
  });
  expect(screen.queryByRole("dialog", { name: "Replace the unfinished import?" })).toBeNull();
  await screen.findByText(/Choose fewer files/, {}, { timeout: 10000 });
  expect.soft(reads.mock.calls.length).toBe(0);
  expect.soft(replace.mock.calls.length).toBe(0);
  expect.soft(await storage.load()).toEqual(checkpoint);
}, 15000);

it("keeps the selection for retry when saving conflicts after planning", async () => {
  const module = await import("./photoImportStorage.js");
  const createStorage = module.photoImportStorage;
  const replace = vi
    .fn()
    .mockRejectedValueOnce(new APIError("Import changed", 409, "checkpoint_changed"));
  vi.spyOn(module, "photoImportStorage").mockImplementation((id, session) => ({
    ...createStorage(id, session),
    replace,
  }));
  const channel: PhotoImportTransport = {
    planPhotoImport: vi
      .fn()
      .mockResolvedValue({ import_id: "retry", groups: [[0]], unsupported: [] }),
    uploadPhotoGroup: vi.fn(),
  };
  render(PhotoImportDrawer, {
    channel,
    vaultID: "vault-save",
    session: "session",
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
      false,
    ),
  );
  const selected = [files()[0]];
  await fireEvent.change(screen.getByLabelText("Choose photo files"), {
    target: { files: selected },
  });
  await screen.findByText("Import changed");
  expect(channel.planPhotoImport).toHaveBeenCalledOnce();
  expect(channel.uploadPhotoGroup).not.toHaveBeenCalled();
  replace.mockResolvedValue(undefined);
  await fireEvent.input(screen.getByRole("textbox", { name: "Destination" }), {
    target: { value: "/retry" },
  });
  await fireEvent.change(screen.getByRole("textbox", { name: "Destination" }), {
    target: { value: "/retry" },
  });
  await screen.findByRole("button", { name: "Add 1 group" });
  expect(channel.planPhotoImport).toHaveBeenCalledWith(
    selected,
    "/retry",
    expect.any(String),
    expect.any(AbortSignal),
    expect.any(Function),
  );
});

it("loads a checkpoint when the vault identity arrives and routes Resume to files", async () => {
  const { photoImportStorage, photoImportManifest } = await import("./photoImportStorage.js");
  const selected = [files()[0]];
  const checkpoint = {
    sourceID: "source-flat",
    revision: "saved",
    destination: "/photos",
    manifestVersion: 2,
    manifestHash: photoImportManifest(selected).digest,
    pickerKind: "files" as const,
  };
  await photoImportStorage("vault-late", "session").replace(checkpoint);
  const channel: PhotoImportTransport = {
    planPhotoImport: vi.fn(),
    uploadPhotoGroup: vi.fn(),
  };
  const view = render(PhotoImportDrawer, {
    channel,
    vaultID: "",
    session: "session",
    onclose: vi.fn(),
    oncomplete: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(true);
  expect(screen.getByRole("status").textContent).toBe("Waiting for vault identity…");
  await view.rerender({ vaultID: "vault-late" });
  const filePicker = vi.spyOn(
    screen.getByLabelText<HTMLInputElement>("Choose photo files"),
    "click",
  );
  const folderPicker = vi.spyOn(
    screen.getByLabelText<HTMLInputElement>("Choose photo folder"),
    "click",
  );
  await fireEvent.click(await screen.findByRole("button", { name: "Resume import" }));
  expect(screen.queryByRole("status")).toBeNull();
  expect(filePicker).toHaveBeenCalledOnce();
  expect(folderPicker).not.toHaveBeenCalled();
  expect(screen.getByLabelText<HTMLInputElement>("Destination").value).toBe("/photos");
  expect(screen.queryByRole("alert")).toBeNull();
});

it.each([
  { reject: false, sessionChanged: false },
  { reject: true, sessionChanged: false },
  { reject: false, sessionChanged: true },
  { reject: true, sessionChanged: true },
])(
  "ignores an obsolete checkpoint load after session change: %o",
  async ({ reject, sessionChanged }) => {
    const module = await import("./photoImportStorage.js");
    const createStorage = module.photoImportStorage;
    const selected = [files()[0]];
    const saved = {
      sourceID: "current-source",
      revision: "current",
      destination: "/photos",
      manifestVersion: 2,
      manifestHash: module.photoImportManifest(selected).digest,
      pickerKind: "files" as const,
    };
    await createStorage("current", "session").replace(saved);
    let finish!: (value: typeof saved) => void;
    let fail!: (cause: Error) => void;
    vi.spyOn(module, "photoImportStorage").mockImplementation((id, session) => ({
      ...createStorage(id, "session"),
      load:
        id === "obsolete" || session === "obsolete"
          ? () =>
              new Promise((resolve, reject) => {
                finish = resolve;
                fail = reject;
              })
          : createStorage(id, "session").load,
    }));
    const channel: PhotoImportTransport = {
      planPhotoImport: vi
        .fn()
        .mockResolvedValue({ import_id: "new", groups: [[0]], unsupported: [] }),
      uploadPhotoGroup: vi.fn(),
    };
    const view = render(PhotoImportDrawer, {
      channel,
      vaultID: sessionChanged ? "current" : "obsolete",
      session: sessionChanged ? "obsolete" : "session",
      onclose: vi.fn(),
      oncomplete: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await waitFor(() => expect(finish).toBeDefined());
    expect(screen.getByRole("status").textContent).toBe("Loading saved import…");
    await view.rerender({ vaultID: "current", session: "session" });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" }).hasAttribute("disabled")).toBe(
        false,
      ),
    );
    if (reject) fail(new Error("Obsolete load failed"));
    else finish({ ...saved, sourceID: "obsolete-source", destination: "/obsolete" });
    await tick();
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Resume import" }));
    expect(screen.getByLabelText<HTMLInputElement>("Destination").value).toBe("/photos");
    await fireEvent.change(screen.getByLabelText("Choose photo files"), {
      target: { files: selected },
    });
    await screen.findByRole("button", { name: "Add 1 group" });
    expect(channel.planPhotoImport).toHaveBeenCalledWith(
      selected,
      "/photos",
      "current-source",
      expect.any(AbortSignal),
      expect.any(Function),
    );
  },
);

it.each([false, true])(
  "keeps saved progress after a rejected plan, prior selection: %s",
  async (prior) => {
    const { photoImportStorage } = await import("./photoImportStorage.js");
    const storage = photoImportStorage("rejected-plan", "session");
    const saved = {
      sourceID: "selection-a",
      revision: "saved",
      destination: "/old",
      manifestVersion: 2,
      manifestHash: "a".repeat(64),
      pickerKind: "files" as const,
    };
    if (prior) await storage.replace(saved);
    const channel: PhotoImportTransport = {
      planPhotoImport: vi.fn().mockRejectedValue(new APIError("not a directory", 422, "not_dir")),
      uploadPhotoGroup: vi.fn(),
    };
    const props = {
      channel,
      vaultID: "rejected-plan",
      session: "session",
      onclose: vi.fn(),
      oncomplete: vi.fn(),
      onauthfailure: vi.fn(),
    };
    const view = render(PhotoImportDrawer, props);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" })).toHaveProperty(
        "disabled",
        false,
      ),
    );
    await fireEvent.change(screen.getByLabelText("Choose photo files"), {
      target: { files: [files()[0]] },
    });
    if (prior) await fireEvent.click(await screen.findByRole("button", { name: "Replace import" }));
    await screen.findByText("not a directory");
    await view.unmount();
    render(PhotoImportDrawer, props);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Choose files" })).toHaveProperty(
        "disabled",
        false,
      ),
    );
    expect(await storage.load()).toEqual(prior ? saved : undefined);
    expect(screen.queryByRole("button", { name: "Resume import" }) !== null).toBe(prior);
  },
);

it.each(["cancel", "confirm", "stale"])(
  "preserves unfinished imports during replacement: %s",
  async (action) => {
    const { photoImportStorage } = await import("./photoImportStorage.js");
    const storage = photoImportStorage("replace-vault", "session");
    const checkpoint = {
      sourceID: "original-source",
      revision: "saved",
      destination: "/old",
      manifestVersion: 2,
      manifestHash: "a".repeat(64),
      pickerKind: "files" as const,
    };
    await storage.replace(checkpoint);
    const channel: PhotoImportTransport = {
      planPhotoImport: vi
        .fn()
        .mockResolvedValue({ import_id: "replacement", groups: [[0]], unsupported: [] }),
      uploadPhotoGroup: vi.fn(),
    };
    render(PhotoImportDrawer, {
      channel,
      vaultID: "replace-vault",
      session: "session",
      onclose: vi.fn(),
      oncomplete: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await screen.findByRole("button", { name: "Resume import" });
    const selected = [files()[0]];
    await fireEvent.change(screen.getByLabelText("Choose photo files"), {
      target: { files: selected },
    });
    const dialog = await screen.findByRole("dialog", { name: "Replace the unfinished import?" });
    expect(dialog.textContent).toContain(
      "The current selection and its progress will be discarded.",
    );
    expect(channel.planPhotoImport).not.toHaveBeenCalled();
    if (action === "stale")
      await storage.replace({ ...checkpoint, revision: "other-tab" }, checkpoint);
    await fireEvent.click(
      screen.getByRole("button", { name: action === "cancel" ? "Cancel" : "Replace import" }),
    );
    if (action === "confirm") {
      await screen.findByRole("button", { name: "Add 1 group" });
      const replacement = await storage.load();
      expect(replacement?.sourceID).not.toBe(checkpoint.sourceID);
      expect(channel.planPhotoImport).toHaveBeenCalledWith(
        selected,
        "/",
        replacement?.sourceID,
        expect.any(AbortSignal),
        expect.any(Function),
      );
    } else if (action === "stale") {
      await screen.findByText(
        "Import progress changed in another tab. Close and reopen Add photos.",
      );
      expect((await storage.load())?.revision).toBe("other-tab");
      expect(channel.planPhotoImport).toHaveBeenCalledOnce();
    } else {
      expect(await storage.load()).toEqual(checkpoint);
      expect(channel.planPhotoImport).not.toHaveBeenCalled();
    }
  },
);
