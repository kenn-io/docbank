import { batchTagRequestDigest, type BatchTagRequest } from "../batch-tags.js";

export async function receiptFor(request: BatchTagRequest, unchanged: number[] = []) {
  return {
    version: 1,
    operation_id: request.operation_id,
    request_digest: await batchTagRequestDigest(request),
    tag_id: request.tag_id,
    assign: request.assign,
    tag_revision: 3,
    assignment_count: 13,
    completed_at: "2026-09-11T13:00:00.000000000Z",
    nodes: request.nodes.map((node) => ({
      node_id: node.node_id,
      expected_revision: node.revision,
      revision: unchanged.includes(node.node_id) ? node.revision : node.revision + 1,
      changed: !unchanged.includes(node.node_id),
    })),
  };
}
