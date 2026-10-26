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
        return Response.json({ id: 7, name: "Reports", kind: "dir", path: "/Reports" });
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
