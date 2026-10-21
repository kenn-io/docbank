import { APIError } from "./api-transport.js";
import {
  reportTelemetryEvent,
  type TelemetryEventPropertiesSearchType,
} from "./generated/docbank.js";

export function reportAction(
  session: string,
  event: "document_added" | "search_run" | "document_opened" | "search_empty" | "error_shown",
  searchType?: string,
  kind?: string,
): void {
  const search_type = (searchType === "lexical" ? "text" : searchType) as
    | TelemetryEventPropertiesSearchType
    | undefined;
  void reportTelemetryEvent(
    { event, properties: { surface: "web", search_type, kind } },
    { session, signal: AbortSignal.timeout(3000) },
  ).catch(() => {});
}

export function reportFailure(session: string, kind: string, cause?: unknown): void {
  if (cause instanceof APIError && cause.status === 401) return;
  if (cause instanceof DOMException && cause.name === "AbortError") return;
  reportAction(session, "error_shown", undefined, kind);
}

export function reportSearch(session: string, type: string, empty: boolean): void {
  reportAction(session, "search_run", type);
  if (empty) reportAction(session, "search_empty", type);
}
