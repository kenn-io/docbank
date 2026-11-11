import { APIError } from "./api-transport.js";
import type { BatchTagRequest } from "./batch-tags.js";
import { getNode } from "./generated/docbank.js";
import { liveNodeTags } from "./receipts.js";

export interface TagResult {
  node_id: number;
  name: string;
  revision?: number;
  status: "matched" | "remaining" | "trash" | "unavailable";
}

export async function checkTagResults(
  session: string,
  request: BatchTagRequest,
  progress: (count: number) => void,
): Promise<TagResult[]> {
  const rows: TagResult[] = new Array(request.nodes.length);
  let next = 0;
  let checked = 0;
  async function worker(): Promise<void> {
    while (next < request.nodes.length) {
      const index = next++;
      const id = request.nodes[index].node_id;
      try {
        const { node, items } = await liveNodeTags(session, id);
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
        if (cause instanceof APIError && cause.status === 401) throw cause;
        try {
          const node = await getNode(id, { session });
          rows[index] = {
            node_id: id,
            name: node.path || `${node.name} (Document ${id})`,
            status: node.trashed_at ? "trash" : "unavailable",
          };
        } catch (metadataCause) {
          if (metadataCause instanceof APIError && metadataCause.status === 401)
            throw metadataCause;
          rows[index] = { node_id: id, name: `Document ${id}`, status: "unavailable" };
        }
      }
      progress(++checked);
    }
  }
  await Promise.all(Array.from({ length: Math.min(8, request.nodes.length) }, () => worker()));
  rows.sort((a, b) => Number(b.status === "remaining") - Number(a.status === "remaining"));
  return rows;
}
