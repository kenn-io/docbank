import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { parseNodeVersionLink, resolveNodeVersionLink } from "./nodeVersionLink.js";

import { node, root, version as contentVersion } from "./version-test-fixtures.js";

afterEach(() => vi.restoreAllMocks());

const version = "11111111-1111-4111-8111-111111111111";

describe("exact version fragments", () => {
  it("accepts a positive safe node ID and a UUID alongside other intent", () => {
    expect(parseNodeVersionLink(`#query=%7B%7D&node=42&version=${version}&photo=image`)).toEqual({
      nodeId: 42,
      versionId: version,
    });
    expect(parseNodeVersionLink(`#node=9007199254740991&version=${version}`)).toEqual({
      nodeId: Number.MAX_SAFE_INTEGER,
      versionId: version,
    });
  });

  it.each(["invalid", "", "../content", "UPPERCASE"])(
    "leaves version policy to the server: %s",
    (versionId) => {
      expect(parseNodeVersionLink(`#node=42&version=${encodeURIComponent(versionId)}`)).toEqual({
        nodeId: 42,
        versionId,
      });
    },
  );

  it.each(["", "#query=%7B%7D", "#photo=image"])("ignores unrelated fragment %s", (hash) => {
    expect(parseNodeVersionLink(hash)).toBeNull();
  });

  it.each([
    `#node=42`,
    `#version=${version}`,
    `#node=&version=${version}`,
    `#node=0&version=${version}`,
    `#node=-1&version=${version}`,
    `#node=1e2&version=${version}`,
    `#node=9007199254740992&version=${version}`,
    `#node=42&node=43&version=${version}`,
    `#node=42&version=${version}&version=${version}`,
  ])("rejects malformed recognized fragment %s", (hash) => {
    expect(parseNodeVersionLink(hash)).toEqual({ error: "invalid" });
  });
});

it.each(["wrong node", "directory"])(
  "rejects a linked %s without current substitution",
  async (kind) => {
    const oldVersion = contentVersion(version, 1, "content_create", "a");
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      if (String(input) === "/api/v1/nodes/42")
        return Response.json(kind === "directory" ? { ...root, id: 42 } : node);
      if (String(input) === `/api/v1/versions/${version}`)
        return Response.json({ ...oldVersion, node_id: 99 });
      throw new Error(`Unexpected request: ${input}`);
    });
    await expect(
      resolveNodeVersionLink("synthetic", { nodeId: 42, versionId: version }),
    ).rejects.toThrow(
      kind === "directory"
        ? "The linked node is a directory. Version links require a file."
        : "The linked version does not belong to the requested file node.",
    );
  },
);
