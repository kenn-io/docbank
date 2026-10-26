import { afterEach, expect, it, vi } from "vite-plus/test";
import { uploadParent } from "./uploadFolders.js";

afterEach(() => vi.restoreAllMocks());

it("finds a matching existing folder beyond the first page by parent ID", async () => {
  const file = new File(["report"], "report.txt");
  Object.defineProperty(file, "webkitRelativePath", { value: "Reports/report.txt" });
  const reads: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    if (init?.method === "POST") return Response.json({ code: "exists" }, { status: 409 });
    const url = String(input);
    reads.push(url);
    return Response.json({
      items: url.includes("offset=0")
        ? Array.from({ length: 1000 }, (_, index) => ({
            id: index + 10,
            name: `Archive ${index}`,
            kind: "dir",
          }))
        : [{ id: 7, name: "Reports", kind: "dir" }],
      total: 1001,
      limit: 1000,
      offset: url.includes("offset=0") ? 0 : 1000,
    });
  });
  const parent = await uploadParent(
    file,
    3,
    new Map(),
    "session",
    new AbortController().signal,
    () => {},
  );
  expect(parent).toBe(7);
  expect(reads).toEqual([
    "/api/v1/nodes/3/children?limit=1000&offset=0",
    "/api/v1/nodes/3/children?limit=1000&offset=1000",
  ]);
});

it("rejects a file that occupies a required folder name", async () => {
  const file = new File(["report"], "report.txt");
  Object.defineProperty(file, "webkitRelativePath", { value: "Reports/report.txt" });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_input, init) =>
    init?.method === "POST"
      ? Response.json({ code: "exists" }, { status: 409 })
      : Response.json({ items: [{ id: 7, name: "Reports", kind: "file" }], total: 1 }),
  );
  await expect(
    uploadParent(file, 3, new Map(), "session", new AbortController().signal, () => {}),
  ).rejects.toThrow("A file already uses the folder name Reports.");
});
