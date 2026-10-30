import { parseMediaType } from "./query.js";
import type { SelectedSource } from "./selectedSource.js";

export function isMarkdown(source: Pick<SelectedSource, "mimeType" | "name">): boolean {
  const mime = parseMediaType(source.mimeType);
  return (
    mime === "text/markdown" ||
    mime === "text/x-markdown" ||
    (mime === "text/plain" && /\.(md|markdown)$/i.test(source.name))
  );
}

export function hasDocumentReader(source: Pick<SelectedSource, "mimeType" | "name">): boolean {
  return parseMediaType(source.mimeType) === "application/pdf" || isMarkdown(source);
}
