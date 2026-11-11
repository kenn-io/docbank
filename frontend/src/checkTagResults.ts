import { APIError } from "./api-transport.js";
import { previewBatchTags, type BatchTagRequest } from "./batch-tags.js";
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
): Promise<{ rows: TagResult[]; complete: boolean }> {
  const rows: TagResult[] = new Array(request.nodes.length);
  let next = 0;
  let checked = 0;
  let complete = true;
  async function worker(): Promise<void> {
    while (next < request.nodes.length) {
      const index = next++;
      const id = request.nodes[index].node_id;
      let name = `Document ${id}`;
      try {
        const node = await getNode(id, { session });
        name = node.name;
        if (node.trashed_at) {
          rows[index] = { node_id: id, name, status: "trash" };
        } else {
          const current = await liveNodeTags(session, id);
          const assigned = current.items.some((tag) => tag.id === request.tag_id);
          rows[index] = {
            node_id: id,
            name: current.node.name,
            revision: current.node.revision,
            status: assigned === request.assign ? "matched" : "remaining",
          };
        }
      } catch (cause) {
        if (cause instanceof APIError && cause.status === 401) throw cause;
        if (!(cause instanceof APIError && cause.status === 404)) complete = false;
        rows[index] = { node_id: id, name, status: "unavailable" };
      }
      progress(++checked);
    }
  }
  await Promise.all(Array.from({ length: Math.min(8, request.nodes.length) }, () => worker()));
  const remaining = rows.filter((row) => row.status === "remaining");
  if (complete && remaining.length) {
    const preview = await previewBatchTags(
      session,
      request.tag_id,
      remaining.map((row) => ({ node_id: row.node_id, revision: row.revision! })),
    );
    if (preview.nodes.some((node) => node.assigned === request.assign))
      throw new Error("Some documents changed. Check their tags again.");
  }
  rows.sort((a, b) => Number(b.status === "remaining") - Number(a.status === "remaining"));
  return { rows, complete };
}
