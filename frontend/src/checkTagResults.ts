import { APIError } from "./api-transport.js";
import { previewBatchTags, type BatchTagRequest } from "./batch-tags.js";
import { getNode } from "./generated/docbank.js";

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
  const candidates: { node_id: number; revision: number }[] = [];
  let next = 0;
  let checked = 0;
  async function worker(): Promise<void> {
    while (next < request.nodes.length) {
      const index = next++;
      const id = request.nodes[index].node_id;
      try {
        const node = await getNode(id, { session });
        rows[index] = {
          node_id: id,
          name: node.name,
          status: node.trashed_at ? "trash" : "unavailable",
        };
        if (!node.trashed_at) candidates.push({ node_id: id, revision: node.revision });
      } catch (cause) {
        if (cause instanceof APIError && cause.status === 401) throw cause;
        rows[index] = { node_id: id, name: `Document ${id}`, status: "unavailable" };
      }
      progress(++checked);
    }
  }
  await Promise.all(Array.from({ length: Math.min(8, request.nodes.length) }, () => worker()));
  if (candidates.length) {
    try {
      const preview = await previewBatchTags(session, request.tag_id, candidates);
      const byID = new Map(preview.nodes.map((node) => [node.node_id, node]));
      for (const row of rows) {
        const node = byID.get(row.node_id);
        if (node) {
          row.revision = node.revision;
          row.status = node.assigned === request.assign ? "matched" : "remaining";
        }
      }
    } catch (cause) {
      if (cause instanceof APIError && cause.status === 401) throw cause;
    }
  }
  rows.sort((a, b) => Number(b.status === "remaining") - Number(a.status === "remaining"));
  return rows;
}
