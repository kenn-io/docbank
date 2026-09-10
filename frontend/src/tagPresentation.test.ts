import { describe, expect, it } from "vitest";
import {
  groupTags,
  splitTagName,
  tagColor,
  type PresentedTag,
} from "./tagPresentation.js";
import type { Tag } from "./api.js";

const tag = (id: string, name: string): Tag => ({
  id,
  name,
  revision: 1,
  assignment_count: 0,
});

describe("tagColor", () => {
  it("derives a stable hex color from tag identity instead of its name", () => {
    const first = tag("11111111-1111-4111-8111-111111111111", "reviewed");
    const renamed = { ...first, name: "matter/acme/reviewed" };
    const differentIdentity = tag(
      "22222222-2222-4222-8222-222222222222",
      "reviewed",
    );

    expect(tagColor(first)).toBe("#bc4c00");
    expect(tagColor(renamed)).toBe("#bc4c00");
    expect(tagColor(differentIdentity)).toBe("#57606a");
  });

  it("returns a ColorLabel-compatible neutral hex for a missing identity", () => {
    expect(tagColor({ id: "" })).toBe("#6e7781");
  });
});

describe("splitTagName", () => {
  it.each([
    ["reviewed", null, "reviewed"],
    ["matter/reviewed", "matter", "reviewed"],
    ["matter/acme/reviewed", "matter/acme", "reviewed"],
    ["/reviewed", null, "/reviewed"],
    ["matter/", null, "matter/"],
    ["matter//reviewed", null, "matter//reviewed"],
  ])("splits %s without discarding malformed full names", (name, group, label) => {
    expect(splitTagName(name)).toEqual({ group, label });
  });
});

describe("groupTags", () => {
  it("sorts ungrouped tags and complete-prefix groups without mutating input", () => {
    const input = [
      tag("5", "matter/zeta/hold"),
      tag("4", "zulu"),
      tag("3", "matter/acme/reviewed"),
      tag("2", "alpha"),
      tag("1", "matter/acme/important"),
    ];
    const original = [...input];

    const groups = groupTags(input);

    expect(input).toEqual(original);
    expect(
      groups.map((group) => ({
        name: group.name,
        tags: group.tags.map((item: PresentedTag) => item.tag.name),
      })),
    ).toEqual([
      { name: null, tags: ["alpha", "zulu"] },
      {
        name: "matter/acme",
        tags: ["matter/acme/important", "matter/acme/reviewed"],
      },
      { name: "matter/zeta", tags: ["matter/zeta/hold"] },
    ]);
  });
});
