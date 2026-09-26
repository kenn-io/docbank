import { checkProductionDecision, getProductionDraftAt, type ProductionDecision } from "./api.js";
import type { Frame } from "./embedpdfAdapter.js";

export type SelectionAssessment =
  | { kind: "ready" }
  | { kind: "expansion"; selector: ProductionDecision["selector"] }
  | { kind: "expansion_unavailable"; boxCount: number }
  | { kind: "conflict"; decisionIDs: string[] };

function validExpandedBox(box: NonNullable<ProductionDecision["selector"]["boxes"]>[number], frame: Frame): boolean {
  return box.page === frame.page && box.frame_sha256 === frame.sha256 &&
    [box.x0, box.y0, box.x1, box.y1].every(Number.isSafeInteger) &&
    box.x0 >= 0 && box.y0 >= 0 && box.x1 > box.x0 && box.y1 > box.y0 &&
    box.x1 <= frame.width && box.y1 <= frame.height;
}

/** Bind a proposed decision and any expanded footprint to the selected page. */
export async function assessSelectionDecision(session: string, setID: string, revision: number, etag: number,
  decision: ProductionDecision, frame: Frame, signal: AbortSignal): Promise<SelectionAssessment> {
  const checked = await checkProductionDecision(session, setID, revision, etag, decision, signal);
  if (checked.set_id !== setID || checked.revision !== revision || checked.etag !== etag ||
      checked.member_id !== decision.member_id || checked.decision_id !== decision.id)
    throw new Error("The decision check disagreed with the selected draft or proposal.");
  let result: SelectionAssessment;
  switch (checked.outcome) {
    case "ready":
      if (checked.expanded) throw new Error("The decision check returned an unexpected expanded selection.");
      result = { kind: "ready" };
      break;
    case "decision_conflict":
      if (checked.expanded || !Array.isArray(checked.decision_ids) || checked.decision_ids.length > 16 ||
          checked.decision_ids.some(id => typeof id !== "string"))
        throw new Error("The decision check returned an invalid conflict.");
      result = { kind: "conflict", decisionIDs: checked.decision_ids };
      break;
    case "selection_expansion_required": {
      const count = checked.expanded_box_count ?? 0;
      if (!Number.isSafeInteger(count) || count < 0)
        throw new Error("The decision check returned an invalid expanded selection.");
      if (!checked.expanded) {
        result = { kind: "expansion_unavailable", boxCount: count };
        break;
      }
      const selector = checked.expanded;
      if (selector.kind !== "rectangle" || selector.map_sha256 !== decision.selector.map_sha256 ||
          !Array.isArray(selector.boxes) || selector.boxes.length < 1 || selector.boxes.length > 64 ||
          selector.boxes.length !== count || selector.boxes.some(box => !validExpandedBox(box, frame)) ||
          selector.pages || selector.span || selector.unit_id)
        throw new Error("The expanded selection disagreed with the verified page frame or map.");
      result = { kind: "expansion", selector };
      break;
    }
    default:
      throw new Error("The decision check returned an unknown outcome.");
  }
  const fresh = await getProductionDraftAt(session, setID, revision, signal);
  signal.throwIfAborted();
  if (fresh.set_id !== setID || fresh.revision !== revision || fresh.etag !== etag || fresh.state !== "draft")
    throw new Error("The production draft changed while checking the selection.");
  return result;
}
