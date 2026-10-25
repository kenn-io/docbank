import { APIError } from "./api-transport.js";
import { createNode, listChildren } from "./generated/docbank.js";
import { localFilePath } from "./localFiles.js";

export async function uploadParent(
  file: File,
  rootID: number,
  directories: Map<string, number>,
  session: string,
  signal: AbortSignal,
  oncreate: () => void,
): Promise<number> {
  const parts = localFilePath(file).split("/");
  if (parts.some((part) => !part || part === "." || part === ".."))
    throw new Error(`Invalid file path: ${localFilePath(file)}`);
  parts.pop();
  let parentID = rootID;
  let path = "";
  for (const name of parts) {
    signal.throwIfAborted();
    path = path ? `${path}/${name}` : name;
    const cached = directories.get(path);
    if (cached !== undefined) {
      parentID = cached;
      continue;
    }
    let id: number | undefined;
    try {
      const directory = await createNode(
        { parent_id: parentID, name, kind: "dir" },
        { session, signal },
      );
      id = directory.id;
      oncreate();
    } catch (cause) {
      if (!(cause instanceof APIError) || cause.status !== 409 || cause.code !== "exists")
        throw cause;
      let offset = 0;
      for (;;) {
        const page = await listChildren(parentID, { limit: 1000, offset }, { session, signal });
        const existing = page.items.find((item) => item.name === name);
        if (existing) {
          if (existing.kind !== "dir")
            throw new Error(`A file already uses the folder name ${name}.`);
          id = existing.id;
          break;
        }
        offset += page.items.length;
        if (!page.items.length || offset >= page.total) break;
      }
      if (id === undefined) throw cause;
    }
    directories.set(path, id);
    parentID = id;
  }
  return parentID;
}
