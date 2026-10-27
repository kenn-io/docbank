// Which optional columns a results table has room for. Names keep at least
// MIN_NAME pixels, and columns drop in a fixed order as the table narrows: Type
// first (the icon says it too), then Match, then Modified.
const CHECKBOX = 44;
const SIZE = 76;
const ACTIONS = 44;
const MIN_NAME = 200;
const MODIFIED = 170;
const MATCH = 88;
const TYPE = 90;

export type ResultColumns = { modified: boolean; match: boolean; type: boolean };

export function resultColumns(
  width: number,
  extras: { match: boolean; actions: boolean },
): ResultColumns {
  let room = width - CHECKBOX - SIZE - MIN_NAME - (extras.actions ? ACTIONS : 0);
  const modified = room >= MODIFIED;
  if (modified) room -= MODIFIED;
  const match = extras.match && room >= MATCH;
  if (match) room -= MATCH;
  return { modified, match, type: room >= TYPE };
}
