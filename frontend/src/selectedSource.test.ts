import { describe, expect, it } from "vitest";
import type { Node } from "./api.js";
import type { SnapshotRow } from "./snapshots.js";
import {
  closeDuplicateSource,
  inspectorSource,
  openDuplicateSource,
  selectedSourceFromNode,
  selectedSourceFromSnapshot,
} from "./selectedSource.js";

const node: Node = {
  id: 7,
  parent_id: 2,
  name: "current.txt",
  path: "/records/current.txt",
  kind: "file",
  current_version_id: "12345678-1234-4123-8123-123456789abc",
  blob_hash: "a".repeat(64),
  size: 25,
  mime_type: "text/plain; charset=utf-8",
  revision: 9,
  created_at: "2026-09-10T11:00:00Z",
  modified_at: "2026-09-11T12:00:00Z",
};

const snapshot: SnapshotRow = {
  node_id: 7,
  content_version_id: "abcdefab-cdef-4abc-8def-abcdefabcdef",
  blob_hash: "b".repeat(64),
  size: 12,
  revision: 3,
  name: "original.txt",
  path: "/imports/original.txt",
  mime_type: "text/plain",
  media_family: "text",
  modified_at: "2026-09-01T12:00:00Z",
  sort_key: "original.txt",
  tags: [
    { id: "11111111-1111-4111-8111-111111111111", name: "frozen", revision: 2 },
  ],
  collection_ids: ["22222222-2222-4222-8222-222222222222"],
  display_collection_id: "22222222-2222-4222-8222-222222222222",
  display_collection_label: "September import",
};

describe("selected source normalization", () => {
  it("pins the current legacy row's complete content authority", () => {
    expect(selectedSourceFromNode(node, node.path ?? "")).toEqual({
      kind: "live",
      key: `live:7:${node.current_version_id}:${node.blob_hash}:25`,
      nodeID: 7,
      mutationRevision: 9,
      versionID: node.current_version_id,
      blobHash: node.blob_hash,
      size: 25,
      name: "current.txt",
      path: "/records/current.txt",
      mimeType: "text/plain; charset=utf-8",
      modifiedAt: "2026-09-11T12:00:00Z",
    });
  });

  it("keeps snapshot facts immutable when a live head has different authority", () => {
    const source = selectedSourceFromSnapshot(snapshot, "2026-09-11T12:34:00Z");

    expect(source).toMatchObject({
      kind: "snapshot",
      key: `snapshot:7:${snapshot.content_version_id}:${snapshot.blob_hash}:12`,
      nodeID: 7,
      mutationRevision: 3,
      versionID: snapshot.content_version_id,
      blobHash: snapshot.blob_hash,
      size: 12,
      name: "original.txt",
      path: "/imports/original.txt",
      mimeType: "text/plain",
      modifiedAt: "2026-09-01T12:00:00Z",
      observedAt: "2026-09-11T12:34:00Z",
      originalTags: snapshot.tags,
      collectionIDs: snapshot.collection_ids,
      displayCollectionID: snapshot.display_collection_id,
      displayCollectionLabel: "September import",
    });
    expect(source.versionID).not.toBe(node.current_version_id);
    expect(source.mutationRevision).not.toBe(node.revision);
  });

  it("rejects legacy files without complete selected authority", () => {
    expect(() =>
      selectedSourceFromNode(
        { ...node, blob_hash: undefined },
        node.path ?? "",
      ),
    ).toThrow("complete selected-source authority");
  });

  it("opens and closes an outside duplicate without replacing the frozen selection", () => {
    const frozen = selectedSourceFromSnapshot(snapshot, "2026-09-11T12:34:00Z");
    const duplicate = selectedSourceFromNode(
      {
        ...node,
        id: 12,
        name: "duplicate.txt",
        path: "/outside/duplicate.txt",
        current_version_id: "22222222-2222-4222-8222-222222222222",
        blob_hash: "c".repeat(64),
        size: 7,
        revision: 5,
      },
      "/outside/duplicate.txt",
    );
    const selection = { frozen };

    const opened = openDuplicateSource(selection, duplicate);

    expect(inspectorSource(opened)).toBe(duplicate);
    expect(opened.frozen).toBe(frozen);
    expect(inspectorSource(closeDuplicateSource(opened))).toBe(frozen);
    expect(selection).toEqual({ frozen });
  });
});
