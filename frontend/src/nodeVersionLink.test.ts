import { describe, expect, it } from "vite-plus/test";
import { parseNodeVersionLink } from "./nodeVersionLink.js";

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

  it.each(["", "#query=%7B%7D", "#photo=image"])("ignores unrelated fragment %s", (hash) => {
    expect(parseNodeVersionLink(hash)).toBeNull();
  });

  it.each([
    `#node=42`,
    `#version=${version}`,
    `#node=&version=${version}`,
    `#node=0&version=${version}`,
    `#node=-1&version=${version}`,
    `#node=1.5&version=${version}`,
    `#node=1e2&version=${version}`,
    `#node=9007199254740992&version=${version}`,
    `#node=42&node=43&version=${version}`,
    `#node=42&version=${version}&version=${version}`,
    "#node=42&version=invalid",
    "#node=42&version=",
    "#node=42&version=../content",
  ])("rejects malformed recognized fragment %s", (hash) => {
    expect(parseNodeVersionLink(hash)).toEqual({ error: "invalid" });
  });
});
