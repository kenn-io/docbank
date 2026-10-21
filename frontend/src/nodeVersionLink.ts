import { getContentVersion, getNode, type ContentVersion, type Node } from "./generated/docbank.js";

export type NodeVersionSelection = { nodeId: number; versionId: string };

export function parseNodeVersionLink(
  hash: string,
): NodeVersionSelection | { error: "invalid" } | null {
  const params = new URLSearchParams(hash.replace(/^#/, ""));
  if (!params.has("node") && !params.has("version")) return null;
  const nodes = params.getAll("node");
  const versions = params.getAll("version");
  const nodeId = Number(nodes[0]);
  if (
    nodes.length !== 1 ||
    versions.length !== 1 ||
    !/^[1-9][0-9]*$/.test(nodes[0]) ||
    !Number.isSafeInteger(nodeId)
  )
    return { error: "invalid" };
  return { nodeId, versionId: versions[0] };
}

export async function resolveNodeVersionLink(
  session: string,
  selection: NodeVersionSelection,
  signal?: AbortSignal,
): Promise<{ node: Node; version: ContentVersion }> {
  const [node, version] = await Promise.all([
    getNode(selection.nodeId, { session, signal }),
    getContentVersion(selection.versionId, { session, signal }),
  ]);
  if (node.kind !== "file" || version.node_id !== node.id) {
    throw new Error("The linked version does not belong to the requested file node.");
  }
  return { node, version };
}
