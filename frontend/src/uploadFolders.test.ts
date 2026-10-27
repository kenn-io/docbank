import { afterEach, expect, it, vi } from "vite-plus/test";
import { uploadParent } from "./uploadFolders.js";

afterEach(() => vi.restoreAllMocks());

it.each([
  { scenario: "reuse", rootPath: "/Archive", parentID: 3, kind: "dir", error: "" },
  {
    scenario: "wrong parent",
    rootPath: "/Archive",
    parentID: 4,
    kind: "dir",
    error: "The parent of folder Reports changed. Try uploading again.",
  },
  {
    scenario: "file in the way",
    rootPath: "/",
    parentID: 3,
    kind: "file",
    error: "A file already uses the folder name Reports.",
  },
])(
  "resolves a folder creation conflict: $scenario",
  async ({ rootPath, parentID, kind, error }) => {
    const file = new File(["report"], "report.txt");
    Object.defineProperty(file, "webkitRelativePath", { value: "Reports/report.txt" });
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      if (init?.method === "POST") {
        expect(JSON.parse(String(init.body))).toEqual({
          parent_id: 3,
          name: "Reports",
          kind: "dir",
        });
        return Response.json({ code: "exists" }, { status: 409 });
      }
      expect(String(input)).toBe(
        rootPath === "/"
          ? "/api/v1/path?path=%2FReports"
          : "/api/v1/path?path=%2FArchive%2FReports",
      );
      return Response.json({
        id: 7,
        parent_id: parentID,
        name: "Reports",
        kind,
        path: `${rootPath === "/" ? "" : rootPath}/Reports`,
      });
    });
    const directories = new Map<string, number>();
    const oncreate = vi.fn();
    const parent = uploadParent(
      file,
      { id: 3, path: rootPath },
      directories,
      "session",
      new AbortController().signal,
      oncreate,
    );
    if (error) {
      await expect(parent).rejects.toThrow(error);
      expect(directories.size).toBe(0);
    } else {
      expect(await parent).toBe(7);
      expect(directories.get("Reports")).toBe(7);
    }
    expect(fetchSpy).toHaveBeenCalledTimes(2);
    expect(oncreate).not.toHaveBeenCalled();
  },
);

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
