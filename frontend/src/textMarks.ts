import type { HighlightTerm } from "./query.js";

export type TextMarkSource = "find" | "highlight" | "query";

export interface TextMatch {
  start: number;
  end: number;
  text: string;
  source: TextMarkSource;
  color: string;
}

export interface TextSegment {
  text: string;
  match?: TextMatch & { index: number };
}

export interface TextMarkResult {
  segments: TextSegment[];
  matches: TextMatch[];
  truncated: boolean;
}

interface MarkOptions {
  find?: string;
  highlightTerms?: HighlightTerm[];
  queryTerms?: string[];
}

interface Candidate extends TextMatch { priority: number }

const maxMatches = 5_000;
const maxCandidatesPerTerm = maxMatches + 1;

export function markText(text: string, options: MarkOptions): TextMarkResult {
  const terms: { text: string; color: string; source: TextMarkSource; priority: number }[] = [];
  if (options.find) terms.push({ text: options.find, color: "#f97316", source: "find", priority: 0 });
  for (const [index, term] of (options.highlightTerms ?? []).entries()) {
    terms.push({ ...term, source: "highlight", priority: 100 + index });
  }
  for (const [index, term] of (options.queryTerms ?? []).entries()) {
    terms.push({ text: term, color: "#facc15", source: "query", priority: 200 + index });
  }

  const candidates: Candidate[] = [];
  for (const term of terms) {
    if ([...term.text].length < 1 || [...term.text].length > 256) continue;
    const expression = new RegExp(escapeRegExp(term.text), "giu");
    let count = 0;
    for (const match of text.matchAll(expression)) {
      if (match.index === undefined) continue;
      candidates.push({
        start: match.index,
        end: match.index + match[0].length,
        text: match[0],
        source: term.source,
        color: term.color,
        priority: term.priority,
      });
      if (++count >= maxCandidatesPerTerm) break;
    }
  }
  candidates.sort((left, right) =>
    left.start - right.start || left.priority - right.priority || right.end - left.end,
  );

  const matches: TextMatch[] = [];
  let cursor = 0;
  let truncated = false;
  for (const candidate of candidates) {
    if (candidate.start < cursor) continue;
    if (matches.length === maxMatches) {
      truncated = true;
      break;
    }
    const { priority: _priority, ...match } = candidate;
    matches.push(match);
    cursor = match.end;
  }

  const segments: TextSegment[] = [];
  cursor = 0;
  matches.forEach((match, index) => {
    if (match.start > cursor) segments.push({ text: text.slice(cursor, match.start) });
    segments.push({ text: text.slice(match.start, match.end), match: { ...match, index } });
    cursor = match.end;
  });
  if (cursor < text.length || segments.length === 0) segments.push({ text: text.slice(cursor) });
  return { segments, matches, truncated };
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
