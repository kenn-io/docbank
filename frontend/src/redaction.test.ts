import { readFileSync } from "node:fs";
import { expect, it } from "vite-plus/test";
import { decodePreparation, decodeExpansion } from "./redaction.js";
import type { RedactionPreparation, RedactionMask } from "./generated/docbank.js";

const fixture = JSON.parse(
  readFileSync("../cmd/docbank/testdata/redaction-preparation.json", "utf8"),
) as RedactionPreparation;
it("binds prepared frames and evidence to the selected source before masks can be edited", () => {
  expect(decodePreparation(fixture, fixture.selection).map.text).toBe("SECRET KEEP SECRET");
  const changed = structuredClone(fixture);
  changed.frames[0]!.width++;
  expect(() => decodePreparation(changed, fixture.selection)).toThrow();
  expect(() => decodePreparation({ ...fixture, unexpected: true }, fixture.selection)).toThrow();
  expect(() => decodePreparation(fixture, { ...fixture.selection, node_id: 999 })).toThrow();
});
it("accepts only the resolver's proposal for a mask in this generation and map", () => {
  const id = "00000000-0000-4000-8000-000000000001";
  const mask: RedactionMask = {
    id,
    selector: { kind: "page", map_sha256: fixture.map.sha256, pages: [1] },
  };
  const expanded = {
    kind: "rectangle",
    map_sha256: fixture.map.sha256,
    boxes: [
      {
        page: 1,
        frame_sha256: fixture.map.pages[0]!.frame_sha256,
        x0: 9000,
        y0: 7800,
        x1: 17000,
        y1: 11000,
      },
    ],
  };
  const problem = {
    code: "selection_expansion_required",
    decision_ids: [id],
    expanded,
    expanded_map_sha256: fixture.map.sha256,
    expanded_boxes: expanded.boxes,
  };
  expect(decodeExpansion(problem, fixture, [mask])).toEqual({ id, selector: expanded });
  expect(() =>
    decodeExpansion(
      { ...problem, decision_ids: ["00000000-0000-4000-8000-000000000002"] },
      fixture,
      [mask],
    ),
  ).toThrow();
  expect(() =>
    decodeExpansion({ ...problem, expanded_map_sha256: "0".repeat(64) }, fixture, [mask]),
  ).toThrow();
  expect(() =>
    decodeExpansion(
      { ...problem, expanded: { ...expanded, boxes: [{ ...expanded.boxes[0], x1: 999999 }] } },
      fixture,
      [mask],
    ),
  ).toThrow();
});
