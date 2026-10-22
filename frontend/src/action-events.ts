import {
  reportTelemetryEvent,
  type TelemetryEventPropertiesSearchType,
} from "./generated/docbank.js";

export function reportAction(
  session: string,
  event: "document_added" | "search_run" | "document_opened",
  searchType?: string,
): void {
  const search_type = (searchType === "lexical" ? "text" : searchType) as
    | TelemetryEventPropertiesSearchType
    | undefined;
  void reportTelemetryEvent(
    { event, properties: { surface: "web", ...(search_type ? { search_type } : {}) } },
    { session, signal: AbortSignal.timeout(3000) },
  ).catch(() => {});
}
