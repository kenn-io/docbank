// Preserve an explicitly supplied empty publication selector. Omitting it
// selects ordinary archive scope, while an empty selector means all notes.
export function publishedNoteParams(params?: object): string {
  const result = new URLSearchParams();
  for (const [key, value] of Object.entries(params ?? {})) {
    if (value === undefined) continue;
    if (
      (key === "published_notes" || key === "note_coverage") &&
      value !== null &&
      typeof value === "object" &&
      !Array.isArray(value)
    ) {
      const entries = Object.entries(value).filter(([, item]) => item !== undefined);
      if (key === "published_notes" && !entries.some(([name]) => name === "publication")) {
        result.append("published_notes[publication]", "");
      }
      if (key === "note_coverage" && entries.length === 0) {
        result.append("note_coverage[profile]", "");
      }
      for (const [name, item] of entries) {
        result.append(`${key}[${name}]`, item === null ? "null" : String(item));
      }
    } else if (Array.isArray(value)) {
      for (const item of value) {
        result.append(key, item === null ? "null" : String(item));
      }
    } else {
      result.append(key, value === null ? "null" : String(value));
    }
  }
  return result.toString();
}
