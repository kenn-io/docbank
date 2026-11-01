import { parseMediaType } from "./query.js";
import type { Node } from "./generated/docbank.js";
import { maxPageSourceBytes } from "./pages.js";
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

// Both entry points require loaded live details before offering a mutation.
export function pdfRedactionAction(
  source: SelectedSource | undefined,
  node: Node | undefined,
  needsRefresh: boolean,
): { disabledReason: string } | undefined {
  if (
    !source ||
    source.kind !== "live" ||
    parseMediaType(source.mimeType) !== "application/pdf" ||
    !node ||
    node.id !== source.nodeID ||
    node.kind !== "file" ||
    node.trashed_at
  )
    return;
  return {
    disabledReason: needsRefresh
      ? "Refresh this view first"
      : source.size === 0
        ? "An empty PDF cannot be redacted"
        : source.size > maxPageSourceBytes
          ? "PDF redaction supports files up to 64 MiB"
          : "",
  };
}
