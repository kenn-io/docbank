import { expect, it } from "vite-plus/test";
import { resultColumns } from "./resultColumns.js";

const search = { match: true, actions: false };

it("drops Type, then Match, then Modified as the table narrows", () => {
  expect(resultColumns(700, search)).toEqual({ modified: true, match: true, type: true });
  expect(resultColumns(600, search)).toEqual({ modified: true, match: true, type: false });
  expect(resultColumns(520, search)).toEqual({ modified: true, match: false, type: false });
  expect(resultColumns(400, search)).toEqual({ modified: false, match: false, type: false });
});

it("reserves room for an actions column", () => {
  expect(resultColumns(520, { match: false, actions: true }).modified).toBe(false);
});
