import { APIError } from "./api-transport.js";
import { previewBatchTags, type BatchTagRequest } from "./batch-tags.js";
import { getNode, getTag, type Tag } from "./generated/docbank.js";
import type { SelectionTarget } from "./selection.js";

export interface TagResult {
  node_id: number;
  name: string;
  revision?: number;
  status: "matched" | "remaining" | "unavailable";
}

export async function checkTagResults(
  session: string,
  request: BatchTagRequest & { nodes: SelectionTarget[] },
  progress: (count: number) => void,
  signal?: AbortSignal,
): Promise<{ tag: Tag; rows: TagResult[] }> {
  const tag = await getTag(request.tag_id, { session, signal });
  const controller = new AbortController();
  const readSignal = signal ? AbortSignal.any([signal, controller.signal]) : controller.signal;
  const rows: TagResult[] = new Array(request.nodes.length);
  let next = 0;
  let checked = 0;
  async function worker(): Promise<void> {
    while (next < request.nodes.length && !controller.signal.aborted) {
      signal?.throwIfAborted();
      const index = next++;
      const { node_id: id, name } = request.nodes[index];
      try {
        const node = await getNode(id, { session, signal: readSignal });
        if (
          node.id !== id ||
          !Number.isSafeInteger(node.revision) ||
          node.revision < 1 ||
          node.trashed_at
        )
          throw new Error("Invalid document");
        const preview = await previewBatchTags(
          session,
          request.tag_id,
          [{ node_id: id, revision: node.revision }],
          readSignal,
        );
        rows[index] = {
          node_id: id,
          name: node.path || `${node.name} (Document ${id})`,
          revision: node.revision,
          status: preview.nodes[0].assigned === request.assign ? "matched" : "remaining",
        };
      } catch (cause) {
        signal?.throwIfAborted();
        if (cause instanceof APIError && cause.status === 401) {
          controller.abort();
          throw cause;
        }
        rows[index] = { node_id: id, name, status: "unavailable" };
      }
      progress(++checked);
    }
  }
  await Promise.all(Array.from({ length: Math.min(8, request.nodes.length) }, () => worker()));
  return { tag, rows };
}
