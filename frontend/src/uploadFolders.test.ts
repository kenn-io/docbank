import { afterEach, expect, it, vi } from "vite-plus/test";
import { uploadParent } from "./uploadFolders.js";

afterEach(() => vi.restoreAllMocks());

it.each([
  ["/", "/api/v1/path?path=%2FReports"],
  ["/Archive", "/api/v1/path?path=%2FArchive%2FReports"],
])(
  "resolves an existing folder under %s once without listing its siblings",
  async (rootPath, url) => {
    const file = new File(["report"], "report.txt");
    Object.defineProperty(file, "webkitRelativePath", { value: "Reports/report.txt" });
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      if (init?.method === "POST") return Response.json({ code: "exists" }, { status: 409 });
      if (String(input) === url)
        return Response.json({
          id: 7,
          parent_id: 3,
          name: "Reports",
          kind: "dir",
          path: "/Reports",
        });
      throw new Error(`Unexpected request ${input}`);
    });
    const oncreate = vi.fn();
    const directories = new Map<string, number>();
    expect(
      await uploadParent(
        file,
        { id: 3, path: rootPath },
        directories,
        "session",
        new AbortController().signal,
        oncreate,
      ),
    ).toBe(7);
    expect(directories.get("Reports")).toBe(7);
    expect(fetchSpy).toHaveBeenCalledTimes(2);
    expect(oncreate).not.toHaveBeenCalled();
  },
);

it("rejects a folder resolved under a different parent after a conflict", async () => {
  const file = new File(["report"], "report.txt");
  Object.defineProperty(file, "webkitRelativePath", { value: "Reports/report.txt" });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (init?.method === "POST") {
      expect(JSON.parse(String(init.body))).toEqual({ parent_id: 3, name: "Reports", kind: "dir" });
      return Response.json({ code: "exists" }, { status: 409 });
    }
    expect(String(input)).toBe("/api/v1/path?path=%2FArchive%2FReports");
    return Response.json({
      id: 7,
      parent_id: 4,
      name: "Reports",
      kind: "dir",
      path: "/Archive/Reports",
    });
  });
  const directories = new Map<string, number>();
  await expect(
    uploadParent(
      file,
      { id: 3, path: "/Archive" },
      directories,
      "session",
      new AbortController().signal,
      () => {},
    ),
  ).rejects.toThrow("The parent of folder Reports changed. Try uploading again.");
  expect(directories.size).toBe(0);
});

it("rejects a file that occupies a required folder name", async () => {
  const file = new File(["report"], "report.txt");
  Object.defineProperty(file, "webkitRelativePath", { value: "Reports/report.txt" });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_input, init) =>
    init?.method === "POST"
      ? Response.json({ code: "exists" }, { status: 409 })
      : Response.json({ id: 7, name: "Reports", kind: "file" }),
  );
  await expect(
    uploadParent(
      file,
      { id: 3, path: "/" },
      new Map(),
      "session",
      new AbortController().signal,
      () => {},
    ),
  ).rejects.toThrow("A file already uses the folder name Reports.");
});

it("creates nested folders under the selected root and reuses them for later files", async () => {
  const created: { parent_id: number; name: string; kind: string }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    expect(String(input)).toBe("/api/v1/nodes");
    expect(init?.method).toBe("POST");
    const body = JSON.parse(String(init?.body));
    created.push(body);
    return Response.json({ id: body.name === "Reports" ? 11 : 12, ...body });
  });
  const directories = new Map<string, number>();
  const oncreate = vi.fn();
  for (const name of ["first.txt", "second.txt", "first.txt"]) {
    const file = new File(["a"], name);
    Object.defineProperty(file, "webkitRelativePath", { value: `Reports/2026/${name}` });
    expect(
      await uploadParent(
        file,
        { id: 3, path: "/Work" },
        directories,
        "session",
        new AbortController().signal,
        oncreate,
      ),
    ).toBe(12);
  }
  expect(created).toEqual([
    { parent_id: 3, name: "Reports", kind: "dir" },
    { parent_id: 11, name: "2026", kind: "dir" },
  ]);
  expect(oncreate).toHaveBeenCalledTimes(2);
});
