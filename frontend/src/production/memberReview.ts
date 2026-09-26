import { getProductionDraftAt, resolveProductionPage, type ProductionMember } from "./api.js";

const shaPattern = /^[0-9a-f]{64}$/;

export class StaleMemberReviewError extends Error {
  constructor() { super("The production draft changed while preparing the review declaration."); }
}

/** Resolve the server's current full-member binding before recording a review. */
export async function loadCurrentMemberReviewBinding(session: string, setID: string, revision: number,
  etag: number, member: ProductionMember, signal: AbortSignal): Promise<string> {
  if (!shaPattern.test(member.map_sha256)) throw new Error("This member has no verified map for review.");
  const resolved = await resolveProductionPage(session, setID, revision, etag, member.id, 1, signal);
  if (resolved.set_id !== setID || resolved.revision !== revision || resolved.etag !== etag ||
      resolved.member_id !== member.id || resolved.map_sha256 !== member.map_sha256 ||
      resolved.page?.number !== 1 || !shaPattern.test(resolved.review_binding)) {
    throw new Error("The review binding disagreed with the selected member or map.");
  }
  const fresh = await getProductionDraftAt(session, setID, revision, signal);
  signal.throwIfAborted();
  if (fresh.set_id !== setID || fresh.revision !== revision || fresh.etag !== etag || fresh.state !== "draft") {
    throw new StaleMemberReviewError();
  }
  return resolved.review_binding;
}
