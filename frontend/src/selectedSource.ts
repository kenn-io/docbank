import type { Node } from "./api.js";
import type { SnapshotRow } from "./snapshots.js";

interface SelectedSourceBase {
  readonly key: string;
  readonly nodeID: number;
  readonly mutationRevision: number;
  readonly versionID: string;
  readonly blobHash: string;
  readonly size: number;
  readonly name: string;
  readonly path: string;
  readonly mimeType: string;
  readonly modifiedAt: string;
}

export interface LiveSelectedSource extends SelectedSourceBase {
  readonly kind: "live";
}

export interface SnapshotSelectedSource extends SelectedSourceBase {
  readonly kind: "snapshot";
  readonly observedAt: string;
  readonly originalTags: ReadonlyArray<Readonly<SnapshotRow["tags"][number]>>;
  readonly collectionIDs: readonly string[];
  readonly displayCollectionID?: string;
  readonly displayCollectionLabel?: string | null;
}

export interface RelatedSelectedSource extends SelectedSourceBase {
  readonly kind: "related";
}

export type SelectedSource = LiveSelectedSource | SnapshotSelectedSource | RelatedSelectedSource;

// DB-17 can open a duplicate as a separate inspector context while retaining
// the source selected from the frozen snapshot for an exact return path.
export interface InspectorSourceSelection {
  readonly frozen: SelectedSource;
  readonly duplicate?: SelectedSource;
}

export function inspectorSource(
  selection: InspectorSourceSelection,
): SelectedSource {
  return selection.duplicate ?? selection.frozen;
}

export function openDuplicateSource(
  selection: InspectorSourceSelection,
  duplicate: SelectedSource,
): InspectorSourceSelection {
  return { frozen: selection.frozen, duplicate };
}

export function closeDuplicateSource(
  selection: InspectorSourceSelection,
): InspectorSourceSelection {
  return { frozen: selection.frozen };
}

function key(
  kind: SelectedSource["kind"],
  nodeID: number,
  versionID: string,
  blobHash: string,
  size: number,
): string {
  return `${kind}:${nodeID}:${versionID}:${blobHash}:${size}`;
}

export function selectedSourceFromNode(
  node: Node,
  path: string,
): LiveSelectedSource {
  if (
    node.kind !== "file" ||
    !node.current_version_id ||
    !node.blob_hash ||
    node.revision < 1 ||
    node.size < 0
  ) {
    throw new Error(
      "The selected document does not have complete selected-source authority.",
    );
  }
  return {
    kind: "live",
    key: key(
      "live",
      node.id,
      node.current_version_id,
      node.blob_hash,
      node.size,
    ),
    nodeID: node.id,
    mutationRevision: node.revision,
    versionID: node.current_version_id,
    blobHash: node.blob_hash,
    size: node.size,
    name: node.name,
    path,
    mimeType: node.mime_type ?? "",
    modifiedAt: node.modified_at,
  };
}

export function selectedSourceFromSnapshot(
  row: SnapshotRow,
  observedAt: string,
): SnapshotSelectedSource {
  return {
    kind: "snapshot",
    key: key(
      "snapshot",
      row.node_id,
      row.content_version_id,
      row.blob_hash,
      row.size,
    ),
    nodeID: row.node_id,
    mutationRevision: row.revision,
    versionID: row.content_version_id,
    blobHash: row.blob_hash,
    size: row.size,
    name: row.name,
    path: row.path,
    mimeType: row.mime_type,
    modifiedAt: row.modified_at,
    observedAt,
    originalTags: row.tags.map((tag) => ({ ...tag })),
    collectionIDs: [...row.collection_ids],
    ...(row.display_collection_id === undefined
      ? {}
      : { displayCollectionID: row.display_collection_id }),
    ...(row.display_collection_label === undefined
      ? {}
      : { displayCollectionLabel: row.display_collection_label }),
  };
}
