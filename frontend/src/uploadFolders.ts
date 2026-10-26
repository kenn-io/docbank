import { APIError } from "./api-transport.js";
import { createNode, resolvePath, type Node } from "./generated/docbank.js";
import { localFilePath } from "./localFiles.js";

export async function uploadParent(
  file: File,
  root: Pick<Node, "id" | "path">,
  directories: Map<string, number>,
  session: string,
  signal: AbortSignal,
  oncreate: () => void,
): Promise<number> {
  const parts = localFilePath(file).split("/");
  if (parts.some((part) => !part || part === "." || part === ".."))
    throw new Error(`Invalid file path: ${localFilePath(file)}`);
  parts.pop();
  let parentID = root.id;
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
      const existing = await resolvePath(
        { path: `${root.path === "/" ? "" : root.path}/${path}` },
        { session, signal },
      );
      if (existing.kind !== "dir") throw new Error(`A file already uses the folder name ${name}.`);
      id = existing.id;
    }
    directories.set(path, id);
    parentID = id;
  }
  return parentID;
}
