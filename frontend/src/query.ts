import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import formatMetadata from "../../document/format_metadata.json";

const maxInputBytes = 128 * 1024;
const maxCanonicalBytes = 64 * 1024;
const maxJSONDepth = 16;
const maxSafeInteger = 9_007_199_254_740_991;
const mediaFamilies = [
  "email", "document", "spreadsheet", "presentation", "image", "audio_video",
  "text", "source_code", "web", "calendar", "archive", "cad", "unknown",
] as const;
const textCoverageValues = ["complete", "partial", "failed", "unprocessed", "none", "unavailable"] as const;
const optionalFilterFields = new Set([
  "paths", "exclude_paths", "collection_ids", "exclude_collection_ids", "tag_ids",
  "exclude_tag_ids", "no_tags", "media_families", "mime_types", "extensions",
  "modified_after", "modified_before", "size_min", "size_max", "text_coverage",
  "has_duplicates", "collapse_duplicates",
]);
const uuidV4Pattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const extensionPattern = /^[a-z0-9][a-z0-9_-]{0,31}$/;
const colorPattern = /^#[0-9a-f]{6}$/;
const encoder = new TextEncoder();

export type MediaFamily = typeof mediaFamilies[number];
export type TextCoverage = typeof textCoverageValues[number];

export interface QuerySort {
  field: "name" | "path" | "modified_at" | "size" | "media_type" | "relevance";
  direction: "asc" | "desc";
}

export interface QueryFilters {
  paths?: string[];
  exclude_paths?: string[];
  collection_ids?: string[];
  exclude_collection_ids?: string[];
  tag_ids?: string[];
  exclude_tag_ids?: string[];
  no_tags?: boolean;
  media_families?: MediaFamily[];
  mime_types?: string[];
  extensions?: string[];
  modified_after?: string;
  modified_before?: string;
  size_min?: number;
  size_max?: number;
  text_coverage?: TextCoverage[];
  has_duplicates?: boolean;
  collapse_duplicates?: boolean;
}

export interface Query {
  v: number;
  text: string;
  syntax: "simple" | "advanced";
  mode: "lexical" | "semantic" | "hybrid";
  filters: QueryFilters;
  sort: QuerySort;
}

export interface HighlightTerm {
  text: string;
  color: string;
}

export interface HighlightSet {
  v: number;
  terms: HighlightTerm[];
}

export function parseQuery(raw: string): Query {
  preflightJSON(raw, true);
  const input: unknown = JSON.parse(raw);
  const object = requireObject(input, "query");
  requireOnlyKeys(object, ["v", "text", "syntax", "mode", "filters", "sort"], "query");

  const filtersInput = object.filters === undefined ? {} : requireObject(object.filters, "filters");
  requireOnlyKeys(filtersInput, [...optionalFilterFields], "filters");
  const sortInput = object.sort === undefined ? {} : requireObject(object.sort, "sort");
  requireOnlyKeys(sortInput, ["field", "direction"], "sort");

  const normalized = normalizeQuery({
    v: optionalInteger(object.v, 1, "v"),
    text: optionalString(object.text, "", "text"),
    syntax: optionalString(object.syntax, "simple", "syntax") as Query["syntax"],
    mode: optionalString(object.mode, "lexical", "mode") as Query["mode"],
    filters: parseFilters(filtersInput),
    sort: {
      field: optionalString(sortInput.field, "name", "sort.field") as QuerySort["field"],
      direction: optionalString(sortInput.direction, "asc", "sort.direction") as QuerySort["direction"],
    },
  });
  canonicalQuery(normalized);
  return normalized;
}

export function canonicalQuery(value: Query): string {
  const normalized = normalizeQuery(value);
  const filters: Record<string, unknown> = {};
  if (normalized.filters.collapse_duplicates) filters.collapse_duplicates = true;
  if (normalized.filters.collection_ids?.length) filters.collection_ids = normalized.filters.collection_ids;
  if (normalized.filters.exclude_collection_ids?.length) filters.exclude_collection_ids = normalized.filters.exclude_collection_ids;
  if (normalized.filters.exclude_paths?.length) filters.exclude_paths = normalized.filters.exclude_paths;
  if (normalized.filters.exclude_tag_ids?.length) filters.exclude_tag_ids = normalized.filters.exclude_tag_ids;
  if (normalized.filters.extensions?.length) filters.extensions = normalized.filters.extensions;
  if (normalized.filters.has_duplicates) filters.has_duplicates = true;
  if (normalized.filters.media_families?.length) filters.media_families = normalized.filters.media_families;
  if (normalized.filters.mime_types?.length) filters.mime_types = normalized.filters.mime_types;
  if (normalized.filters.modified_after) filters.modified_after = normalized.filters.modified_after;
  if (normalized.filters.modified_before) filters.modified_before = normalized.filters.modified_before;
  if (normalized.filters.no_tags) filters.no_tags = true;
  if (normalized.filters.paths?.length) filters.paths = normalized.filters.paths;
  if (normalized.filters.size_max !== undefined) filters.size_max = normalized.filters.size_max;
  if (normalized.filters.size_min) filters.size_min = normalized.filters.size_min;
  if (normalized.filters.tag_ids?.length) filters.tag_ids = normalized.filters.tag_ids;
  if (normalized.filters.text_coverage?.length) filters.text_coverage = normalized.filters.text_coverage;
  const encoded = JSON.stringify({
    filters,
    mode: normalized.mode,
    sort: { direction: normalized.sort.direction, field: normalized.sort.field },
    syntax: normalized.syntax,
    text: normalized.text,
    v: normalized.v,
  });
  requireCanonicalBound(encoded, "query");
  return encoded;
}

export async function queryFingerprint(value: Query): Promise<string> {
  return `sha256:${bytesToHex(sha256(utf8ToBytes(canonicalQuery(value))))}`;
}

export function parseHighlightSet(raw: string): HighlightSet {
  preflightJSON(raw, false);
  const input: unknown = JSON.parse(raw);
  const object = requireObject(input, "highlight set");
  requireOnlyKeys(object, ["v", "terms"], "highlight set");
  if (!Array.isArray(object.terms)) throw new Error("highlight terms are required");
  const terms = object.terms.map((value, index) => {
    const term = requireObject(value, `terms[${index}]`);
    requireOnlyKeys(term, ["text", "color"], `terms[${index}]`);
    return {
      text: requireString(term.text, `terms[${index}].text`),
      color: requireString(term.color, `terms[${index}].color`),
    };
  });
  const normalized = normalizeHighlightSet({ v: optionalInteger(object.v, 1, "v"), terms });
  canonicalHighlightSet(normalized);
  return normalized;
}

export function canonicalHighlightSet(value: HighlightSet): string {
  const normalized = normalizeHighlightSet(value);
  const encoded = JSON.stringify({
    terms: normalized.terms.map((term) => ({ color: term.color, text: term.text })),
    v: normalized.v,
  });
  requireCanonicalBound(encoded, "highlight set");
  return encoded;
}

export async function highlightSetFingerprint(value: HighlightSet): Promise<string> {
  return `sha256:${bytesToHex(sha256(utf8ToBytes(canonicalHighlightSet(value))))}`;
}

function parseFilters(input: Record<string, unknown>): QueryFilters {
  return {
    paths: optionalStringArray(input.paths, "filters.paths"),
    exclude_paths: optionalStringArray(input.exclude_paths, "filters.exclude_paths"),
    collection_ids: optionalStringArray(input.collection_ids, "filters.collection_ids"),
    exclude_collection_ids: optionalStringArray(input.exclude_collection_ids, "filters.exclude_collection_ids"),
    tag_ids: optionalStringArray(input.tag_ids, "filters.tag_ids"),
    exclude_tag_ids: optionalStringArray(input.exclude_tag_ids, "filters.exclude_tag_ids"),
    no_tags: optionalBoolean(input.no_tags, "filters.no_tags"),
    media_families: optionalStringArray(input.media_families, "filters.media_families") as MediaFamily[] | undefined,
    mime_types: optionalStringArray(input.mime_types, "filters.mime_types"),
    extensions: optionalStringArray(input.extensions, "filters.extensions"),
    modified_after: optionalNullableString(input.modified_after, "filters.modified_after"),
    modified_before: optionalNullableString(input.modified_before, "filters.modified_before"),
    size_min: optionalNullableInteger(input.size_min, "filters.size_min"),
    size_max: optionalNullableInteger(input.size_max, "filters.size_max"),
    text_coverage: optionalStringArray(input.text_coverage, "filters.text_coverage") as TextCoverage[] | undefined,
    has_duplicates: optionalBoolean(input.has_duplicates, "filters.has_duplicates"),
    collapse_duplicates: optionalBoolean(input.collapse_duplicates, "filters.collapse_duplicates"),
  };
}

function normalizeQuery(value: Query): Query {
  const v = value.v;
  const syntax = value.syntax;
  const mode = value.mode;
  const sort = value.sort;
  if (v !== 1) throw new Error("query version must be 1");
  validateUnicode(value.text, "query text");
  if (scalarLength(value.text) > 8192) throw new Error("query text exceeds 8192 Unicode scalars");
  if (!(["simple", "advanced"] as string[]).includes(syntax)) throw new Error("query syntax is unknown");
  if (!(["lexical", "semantic", "hybrid"] as string[]).includes(mode)) throw new Error("query mode is unknown");
  if (!(["name", "path", "modified_at", "size", "media_type", "relevance"] as string[]).includes(sort.field) ||
      !(["asc", "desc"] as string[]).includes(sort.direction)) throw new Error("query sort is invalid");
  return { v, text: value.text, syntax, mode, filters: normalizeFilters(value.filters ?? {}), sort };
}

function normalizeFilters(value: QueryFilters): QueryFilters {
  const result: QueryFilters = {
    paths: normalizeSet(value.paths, 64, validVirtualPath, "paths"),
    exclude_paths: normalizeSet(value.exclude_paths, 64, validVirtualPath, "exclude_paths"),
    collection_ids: normalizeSet(value.collection_ids, 64, (item) => uuidV4Pattern.test(item), "collection_ids"),
    exclude_collection_ids: normalizeSet(value.exclude_collection_ids, 64, (item) => uuidV4Pattern.test(item), "exclude_collection_ids"),
    tag_ids: normalizeSet(value.tag_ids, 64, (item) => uuidV4Pattern.test(item), "tag_ids"),
    exclude_tag_ids: normalizeSet(value.exclude_tag_ids, 64, (item) => uuidV4Pattern.test(item), "exclude_tag_ids"),
    no_tags: Boolean(value.no_tags) || undefined,
    media_families: normalizeSet(value.media_families, 13, (item) => (mediaFamilies as readonly string[]).includes(item), "media_families") as MediaFamily[] | undefined,
    mime_types: normalizeSet(value.mime_types, 64, validConcreteMIME, "mime_types"),
    extensions: normalizeSet(value.extensions, 32, (item) => extensionPattern.test(item), "extensions"),
    modified_after: normalizeTimestamp(value.modified_after, "modified_after"),
    modified_before: normalizeTimestamp(value.modified_before, "modified_before"),
    size_min: normalizeSize(value.size_min, "size_min") || undefined,
    size_max: normalizeSize(value.size_max, "size_max"),
    text_coverage: normalizeSet(value.text_coverage, 6, (item) => (textCoverageValues as readonly string[]).includes(item), "text_coverage") as TextCoverage[] | undefined,
    has_duplicates: Boolean(value.has_duplicates) || undefined,
    collapse_duplicates: Boolean(value.collapse_duplicates) || undefined,
  };
  if (result.no_tags && result.tag_ids?.length) throw new Error("no_tags conflicts with tag_ids");
  if (result.modified_after && result.modified_before &&
      timestampComparable(result.modified_after) >= timestampComparable(result.modified_before)) {
    throw new Error("modified_after must precede modified_before");
  }
  if (result.size_max !== undefined && (result.size_min ?? 0) > result.size_max) throw new Error("size_min exceeds size_max");
  return result;
}

function normalizeHighlightSet(value: HighlightSet): HighlightSet {
  const v = value.v;
  if (v !== 1) throw new Error("highlight set version must be 1");
  if (!Array.isArray(value.terms) || value.terms.length < 1 || value.terms.length > 64) {
    throw new Error("highlight set must contain 1 to 64 terms");
  }
  const seen = new Set<string>();
  const terms = value.terms.map((term) => {
    validateUnicode(term.text, "highlight text");
    const length = scalarLength(term.text);
    if (length < 1 || length > 256) throw new Error("highlight text is outside its length bounds");
    if (!colorPattern.test(term.color)) throw new Error("highlight color must be lowercase #rrggbb");
    if (seen.has(term.text)) throw new Error("highlight text is duplicated");
    seen.add(term.text);
    return { text: term.text, color: term.color };
  });
  return { v, terms };
}

function normalizeSet(values: readonly string[] | undefined, limit: number, valid: (value: string) => boolean, field: string): string[] | undefined {
  if (values === undefined || values === null) return undefined;
  if (!Array.isArray(values) || values.length > limit) throw new Error(`${field} exceeds its supplied-entry bound`);
  for (const value of values) {
    if (typeof value !== "string") throw new Error(`${field} must contain strings`);
    validateUnicode(value, field);
    if (!valid(value)) throw new Error(`${field} contains an invalid value`);
  }
  const result = [...new Set(values)].sort(compareUnicodeScalars);
  return result.length ? result : undefined;
}

function compareUnicodeScalars(left: string, right: string): number {
  const leftScalars = Array.from(left, (value) => value.codePointAt(0)!);
  const rightScalars = Array.from(right, (value) => value.codePointAt(0)!);
  for (let index = 0; index < Math.min(leftScalars.length, rightScalars.length); index++) {
    if (leftScalars[index] !== rightScalars[index]) return leftScalars[index] - rightScalars[index];
  }
  return leftScalars.length - rightScalars.length;
}

function validVirtualPath(value: string): boolean {
  if (value === "/") return true;
  if (!value.startsWith("/") || value.endsWith("/") || value.includes("\0")) return false;
  return value.slice(1).split("/").every((segment) => segment !== "" && segment !== "." && segment !== "..");
}

function validConcreteMIME(value: string): boolean {
  if (value !== value.toLowerCase()) return false;
  const slash = value.indexOf("/");
  return slash > 0 && slash === value.lastIndexOf("/") &&
    validMIMEToken(value.slice(0, slash), false) && validMIMEToken(value.slice(slash + 1), false);
}

function validMIMEToken(value: string, allowWildcard: boolean): boolean {
  if (value === "") return false;
  for (const character of value) {
    const code = character.charCodeAt(0);
    if (code <= 0x20 || code >= 0x7f || "()<>@,;:\\\"/[]?=".includes(character) ||
        (!allowWildcard && character === "*")) return false;
  }
  return true;
}

function normalizeSize(value: number | undefined, field: string): number | undefined {
  if (value === undefined) return undefined;
  if (!Number.isSafeInteger(value) || value < 0 || value > maxSafeInteger) throw new Error(`${field} is not a safe nonnegative integer`);
  return value;
}

function normalizeTimestamp(value: string | undefined, field: string): string | undefined {
  if (value === undefined) return undefined;
  validateUnicode(value, field);
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/.exec(value);
  if (!match) throw new Error(`${field} is not RFC3339`);
  const [, yearRaw, monthRaw, dayRaw, hourRaw, minuteRaw, secondRaw, fractionRaw = "", zone] = match;
  const [year, month, day, hour, minute, second] = [yearRaw, monthRaw, dayRaw, hourRaw, minuteRaw, secondRaw].map(Number);
  if (month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59) throw new Error(`${field} is not RFC3339`);
  const local = new Date(0);
  local.setUTCHours(hour, minute, second, Number(fractionRaw.padEnd(3, "0").slice(0, 3)));
  local.setUTCFullYear(year, month - 1, day);
  if (local.getUTCFullYear() !== year || local.getUTCMonth() !== month - 1 || local.getUTCDate() !== day) throw new Error(`${field} is not RFC3339`);
  let offsetMinutes = 0;
  if (zone !== "Z") {
    const offsetHour = Number(zone.slice(1, 3));
    const offsetMinute = Number(zone.slice(4, 6));
    if (offsetHour > 23 || offsetMinute > 59) throw new Error(`${field} is not RFC3339`);
    offsetMinutes = (offsetHour * 60 + offsetMinute) * (zone[0] === "+" ? 1 : -1);
  }
  const utc = new Date(local.getTime() - offsetMinutes * 60_000);
  const utcYear = utc.getUTCFullYear();
  if (utcYear < 0 || utcYear > 9999) throw new Error(`${field} normalizes outside RFC3339`);
  const date = `${String(utcYear).padStart(4, "0")}-${String(utc.getUTCMonth() + 1).padStart(2, "0")}-${String(utc.getUTCDate()).padStart(2, "0")}`;
  const clock = `${String(utc.getUTCHours()).padStart(2, "0")}:${String(utc.getUTCMinutes()).padStart(2, "0")}:${String(utc.getUTCSeconds()).padStart(2, "0")}`;
  const fraction = fractionRaw.replace(/0+$/, "");
  return `${date}T${clock}${fraction ? `.${fraction}` : ""}Z`;
}

function timestampComparable(value: string): string {
  return value.replace(/(?:\.(\d+))?Z$/, (_match, fraction = "") => `.${fraction.padEnd(9, "0")}Z`);
}

function requireObject(value: unknown, field: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) throw new Error(`${field} must be an object`);
  return value as Record<string, unknown>;
}

function requireOnlyKeys(value: Record<string, unknown>, allowed: readonly string[], field: string): void {
  const keys = new Set(allowed);
  for (const key of Object.keys(value)) if (!keys.has(key)) throw new Error(`${field} contains an unknown member`);
}

function requireString(value: unknown, field: string): string {
  if (typeof value !== "string") throw new Error(`${field} must be a string`);
  validateUnicode(value, field);
  return value;
}

function optionalString(value: unknown, fallback: string, field: string): string {
  return value === undefined ? fallback : requireString(value, field);
}

function optionalNullableString(value: unknown, field: string): string | undefined {
  return value === undefined || value === null ? undefined : requireString(value, field);
}

function optionalInteger(value: unknown, fallback: number, field: string): number {
  if (value === undefined) return fallback;
  if (typeof value !== "number" || !Number.isSafeInteger(value)) throw new Error(`${field} must be a safe integer`);
  return value;
}

function optionalNullableInteger(value: unknown, field: string): number | undefined {
  return value === undefined || value === null ? undefined : optionalInteger(value, 0, field);
}

function optionalBoolean(value: unknown, field: string): boolean | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== "boolean") throw new Error(`${field} must be a boolean`);
  return value;
}

function optionalStringArray(value: unknown, field: string): string[] | undefined {
  if (value === undefined || value === null) return undefined;
  if (!Array.isArray(value) || !value.every((item) => typeof item === "string")) throw new Error(`${field} must be an array of strings`);
  return [...value];
}

function scalarLength(value: string): number { return Array.from(value).length; }

function validateUnicode(value: string, field: string): void {
  for (let index = 0; index < value.length; index++) {
    const code = value.charCodeAt(index);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(++index);
      if (!(next >= 0xdc00 && next <= 0xdfff)) throw new Error(`${field} contains an invalid Unicode surrogate`);
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      throw new Error(`${field} contains an invalid Unicode surrogate`);
    }
  }
}

function requireCanonicalBound(encoded: string, kind: string): void {
  if (encoder.encode(encoded).byteLength > maxCanonicalBytes) throw new Error(`canonical ${kind} exceeds 64 KiB`);
}

class StrictJSONScanner {
  private index = 0;
  constructor(private readonly input: string, private readonly allowFilterNull: boolean) {}

  scan(): void {
    this.skipWhitespace();
    this.value([], 0);
    this.skipWhitespace();
    if (this.index !== this.input.length) throw new Error("input contains trailing JSON");
  }

  private value(path: string[], depth: number): void {
    this.skipWhitespace();
    const character = this.input[this.index];
    if (character === "{") return this.object(path, depth + 1);
    if (character === "[") return this.array(path, depth + 1);
    if (character === '"') { this.string(); return; }
    if (character === "t") return this.literal("true");
    if (character === "f") return this.literal("false");
    if (character === "n") {
      this.literal("null");
      if (!(this.allowFilterNull && path.length === 2 && path[0] === "filters" && optionalFilterFields.has(path[1]))) {
        throw new Error("null is allowed only for optional query filters");
      }
      return;
    }
    this.number();
  }

  private object(path: string[], depth: number): void {
    if (depth > maxJSONDepth) throw new Error("JSON nesting exceeds 16 levels");
    this.index++;
    this.skipWhitespace();
    const names = new Set<string>();
    if (this.input[this.index] === "}") { this.index++; return; }
    while (true) {
      if (this.input[this.index] !== '"') throw new Error("object name must be a string");
      const name = this.string();
      if (names.has(name)) throw new Error("duplicate object member");
      names.add(name);
      this.skipWhitespace();
      if (this.input[this.index++] !== ":") throw new Error("object member is missing a colon");
      this.value([...path, name], depth);
      this.skipWhitespace();
      const delimiter = this.input[this.index++];
      if (delimiter === "}") return;
      if (delimiter !== ",") throw new Error("object member is missing a delimiter");
      this.skipWhitespace();
    }
  }

  private array(path: string[], depth: number): void {
    if (depth > maxJSONDepth) throw new Error("JSON nesting exceeds 16 levels");
    this.index++;
    this.skipWhitespace();
    if (this.input[this.index] === "]") { this.index++; return; }
    while (true) {
      this.value([...path, "[]"], depth);
      this.skipWhitespace();
      const delimiter = this.input[this.index++];
      if (delimiter === "]") return;
      if (delimiter !== ",") throw new Error("array value is missing a delimiter");
      this.skipWhitespace();
    }
  }

  private string(): string {
    const start = this.index++;
    while (this.index < this.input.length) {
      const code = this.input.charCodeAt(this.index++);
      if (code === 0x22) {
        const value = JSON.parse(this.input.slice(start, this.index)) as string;
        validateUnicode(value, "JSON string");
        return value;
      }
      if (code < 0x20) throw new Error("JSON string contains a control character");
      if (code === 0x5c) {
        const escape = this.input[this.index++];
        if (escape === "u") {
          const hex = this.input.slice(this.index, this.index + 4);
          if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error("JSON string contains an invalid escape");
          this.index += 4;
        } else if (!'"\\/bfnrt'.includes(escape ?? "")) {
          throw new Error("JSON string contains an invalid escape");
        }
      }
    }
    throw new Error("JSON string is unterminated");
  }

  private literal(literal: string): void {
    if (this.input.slice(this.index, this.index + literal.length) !== literal) throw new Error("invalid JSON literal");
    this.index += literal.length;
  }

  private number(): void {
    const match = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(this.input.slice(this.index));
    if (!match) throw new Error("invalid JSON value");
    const lexeme = match[0];
    this.index += lexeme.length;
    if (/[.eE]/.test(lexeme)) throw new Error("JSON numbers must use whole-number lexemes");
    if (!Number.isSafeInteger(Number(lexeme))) throw new Error("JSON integer exceeds the safe range");
  }

  private skipWhitespace(): void {
    while (/[\t\n\r ]/.test(this.input[this.index] ?? "")) this.index++;
  }
}

function preflightJSON(raw: string, allowFilterNull: boolean): void {
  if (encoder.encode(raw).byteLength > maxInputBytes) throw new Error("input exceeds 128 KiB");
  new StrictJSONScanner(raw, allowFilterNull).scan();
}

const queryFamilyByMIME = new Map(formatMetadata.map((entry) => [entry.media_type, entry.query_family as MediaFamily]));
const queryFamilyByExtension = new Map(formatMetadata.flatMap((entry) => entry.extensions.map((extension) => [extension, entry.query_family as MediaFamily] as const)));

export function classifyMedia(mediaType: string, filename: string): MediaFamily {
  const trimmed = trimASCIIWhitespace(mediaType);
  if (trimmed !== "") {
    const essence = parseMediaType(trimmed);
    if (essence === null) return "unknown";
    if (essence !== "application/octet-stream") {
      const exact = queryFamilyByMIME.get(essence);
      if (exact) return exact;
      if (essence.startsWith("image/")) return "image";
      if (essence.startsWith("audio/") || essence.startsWith("video/")) return "audio_video";
      return "unknown";
    }
  }
  const base = filename.slice(Math.max(filename.lastIndexOf("/"), filename.lastIndexOf("\\")) + 1);
  const dot = base.lastIndexOf(".");
  if (dot <= 0 || dot === base.length - 1) return "unknown";
  const extension = lowerASCII(base.slice(dot + 1));
  return extension === null ? "unknown" : queryFamilyByExtension.get(extension) ?? "unknown";
}

function parseMediaType(value: string): string | null {
  const semicolon = value.indexOf(";");
  const essence = lowerASCII(trimASCIIWhitespace(value.slice(0, semicolon < 0 ? undefined : semicolon)));
  if (essence === null) return null;
  if (!validConcreteMIME(essence)) return null;
  if (semicolon < 0) return essence;
  let index = semicolon;
  const names = new Set<string>();
  while (index < value.length) {
    if (value[index++] !== ";") return null;
    while (value[index] === " " || value[index] === "\t") index++;
    const nameStart = index;
    while (validMIMEToken(value[index] ?? "", true)) index++;
    const name = value.slice(nameStart, index).toLowerCase();
    if (!name || names.has(name)) return null;
    names.add(name);
    while (value[index] === " " || value[index] === "\t") index++;
    if (value[index++] !== "=") return null;
    while (value[index] === " " || value[index] === "\t") index++;
    if (value[index] === '"') {
      index++;
      let closed = false;
      while (index < value.length) {
        const character = value[index++];
        if (character === '"') { closed = true; break; }
        if (character === "\\") {
          if (index >= value.length || invalidMIMEQuotedCharacter(value[index])) return null;
          index++;
        } else if (invalidMIMEQuotedCharacter(character)) return null;
      }
      if (!closed) return null;
    } else {
      const valueStart = index;
      while (validMIMEToken(value[index] ?? "", true)) index++;
      if (index === valueStart) return null;
    }
    while (value[index] === " " || value[index] === "\t") index++;
    if (index < value.length && value[index] !== ";") return null;
  }
  return essence;
}

function trimASCIIWhitespace(value: string): string {
  return value.replace(/^[\t\n\v\f\r ]+|[\t\n\v\f\r ]+$/g, "");
}

function lowerASCII(value: string): string | null {
  let result = "";
  for (let index = 0; index < value.length; index++) {
    const code = value.charCodeAt(index);
    if (code >= 0x80) return null;
    result += code >= 0x41 && code <= 0x5a ? String.fromCharCode(code + 0x20) : value[index];
  }
  return result;
}

function invalidMIMEQuotedCharacter(character: string): boolean {
  const code = character.charCodeAt(0);
  return code < 0x20 && character !== "\t" || code === 0x7f;
}
