import { getProductionDraftAt, type ProductionDraft, type ProductionMember } from "./api.js";

const shaPattern = /^[0-9a-f]{64}$/;

export class StaleMembershipSealError extends Error {
  constructor() { super("The production draft changed while preparing the membership seal."); }
}

/** Use the server's digest and a complete, ordered member list for the exact draft. */
export async function prepareProductionMemberSeal(session: string, draft: ProductionDraft,
  members: ProductionMember[], nextCursor: string, signal: AbortSignal): Promise<{ total: number; memberHash: string }> {
  if (nextCursor) throw new Error("Load all members before sealing membership.");
  if (!members.length || members.length > 100_000 || !shaPattern.test(draft.member_hash)) {
    throw new Error("This draft has no valid membership to seal.");
  }
  const ids = new Set<string>();
  for (const [index, member] of members.entries()) {
    if (member.ordinal !== index + 1 || ids.has(member.id)) throw new Error("Members must be ordered without duplicates.");
    ids.add(member.id);
  }
  const fresh = await getProductionDraftAt(session, draft.set_id, draft.revision, signal);
  signal.throwIfAborted();
  if (fresh.set_id !== draft.set_id || fresh.revision !== draft.revision || fresh.etag !== draft.etag ||
      fresh.member_hash !== draft.member_hash || fresh.state !== "draft" || fresh.membership_sealed) {
    throw new StaleMembershipSealError();
  }
  return { total: members.length, memberHash: draft.member_hash };
}
