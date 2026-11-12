import { APIError } from "./api-transport.js";
import type { BatchTagRequest } from "./batch-tags.js";
import { getTag, type Tag } from "./generated/docbank.js";
import { liveNodeTags } from "./receipts.js";

export interface TagResult {
  node_id: number;
  name: string;
  revision?: number;
  status: "matched" | "remaining" | "unavailable";
}

export async function checkTagResults(
  session: string,
  request: BatchTagRequest,
  progress: (count: number) => void,
  signal?: AbortSignal,
): Promise<{ tag: Tag; rows: TagResult[] }> {
  const tag = await getTag(request.tag_id, { session, signal });
  const controller = new AbortController();
  const readSignal = signal ? AbortSignal.any([signal, controller.signal]) : controller.signal;
  const rows: TagResult[] = new Array(request.nodes.length);
  let next = 0;
  let checked = 0;
  let authFailure: unknown;
  async function worker(): Promise<void> {
    while (next < request.nodes.length && !authFailure) {
      signal?.throwIfAborted();
      const index = next++;
      const id = request.nodes[index].node_id;
      try {
        const { node, items } = await liveNodeTags(session, id, readSignal);
        rows[index] = {
          node_id: id,
          name: node.path || `${node.name} (Document ${id})`,
          revision: node.revision,
          status:
            items.some((tag) => tag.id === request.tag_id) === request.assign
              ? "matched"
              : "remaining",
        };
      } catch (cause) {
        signal?.throwIfAborted();
        if (cause instanceof APIError && cause.status === 401) {
          authFailure = cause;
          controller.abort();
          throw cause;
        }
        rows[index] = { node_id: id, name: `Document ${id}`, status: "unavailable" };
      }
      progress(++checked);
    }
  }
  await Promise.all(Array.from({ length: Math.min(8, request.nodes.length) }, () => worker()));
  rows.sort((a, b) => Number(b.status === "remaining") - Number(a.status === "remaining"));
  return { tag, rows };
}
