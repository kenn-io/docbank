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

async function readTarget(
  session: string,
  { node_id: id }: SelectionTarget,
  signal: AbortSignal,
): Promise<TagResult> {
  const node = await getNode(id, { session, signal });
  if (node.id !== id) throw new Error("Invalid document");
  const path = node.path || `${node.name} (Document ${id})`;
  return {
    node_id: id,
    name: path,
    revision: node.revision,
    status: "remaining",
  };
}

// Runs task for each index with at most eight in flight, and starts no more after a failure or abort.
async function pool(count: number, signal: AbortSignal, task: (index: number) => Promise<void>) {
  let next = 0;
  let failed = false;
  const worker = async () => {
    while (next < count && !failed && !signal.aborted)
      await task(next++).catch((cause) => {
        failed = true;
        throw cause;
      });
  };
  await Promise.all(Array.from({ length: Math.min(8, count) }, worker));
}

const authFailure = (cause: unknown) => cause instanceof APIError && cause.status === 401;

// Previews every readable document in one request. If that is refused, previews them one by one
// so a single stale document leaves the other results intact.
async function previewAssigned(
  session: string,
  tagID: string,
  targets: { node_id: number; revision: number }[],
  signal: AbortSignal,
): Promise<Map<number, boolean>> {
  const assigned = new Map<number, boolean>();
  const preview = async (group: typeof targets): Promise<void> => {
    try {
      const result = await previewBatchTags(session, tagID, group, signal);
      for (const node of result.nodes) assigned.set(node.node_id, node.assigned);
    } catch (cause) {
      if (signal.aborted || authFailure(cause)) throw cause;
      if (group.length > 1) await pool(group.length, signal, (index) => preview([group[index]]));
    }
  };
  if (targets.length > 0) await preview(targets);
  return assigned;
}

function classify(
  row: TagResult,
  { node_id, name }: SelectionTarget,
  assigned: boolean | undefined,
  want: boolean,
): TagResult {
  if (row.status === "unavailable") return row;
  if (assigned === undefined) return { node_id, name, status: "unavailable" };
  return { ...row, status: assigned === want ? "matched" : "remaining" };
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
  const rows = Array.from<TagResult>({ length: request.nodes.length });
  let checked = 0;
  await pool(request.nodes.length, readSignal, async (index) => {
    const { node_id, name } = request.nodes[index];
    try {
      rows[index] = await readTarget(session, request.nodes[index], readSignal);
    } catch (cause) {
      signal?.throwIfAborted();
      if (authFailure(cause)) {
        controller.abort();
        throw cause;
      }
      rows[index] = { node_id, name, status: "unavailable" };
    }
    progress(++checked);
  });
  signal?.throwIfAborted();
  const readable = rows.filter((row) => row.status !== "unavailable");
  const assigned = await previewAssigned(
    session,
    request.tag_id,
    readable.map(({ node_id, revision }) => ({ node_id, revision: revision! })),
    readSignal,
  );
  return {
    tag,
    rows: rows.map((row, index) =>
      classify(row, request.nodes[index], assigned.get(row.node_id), request.assign),
    ),
  };
}
