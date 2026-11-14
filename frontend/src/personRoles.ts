import type { PersonRole } from "./query.js";

export type MatchedRole = PersonRole | "mentioned";

const fixedLabels: Partial<Record<MatchedRole, string>> = {
  mentioned: "Mentioned",
  copied: "Cc",
  blind_copy: "Bcc",
};

// How a person's role on a document reads in result chips and the People facet.
export function roleChipLabel(role: MatchedRole): string {
  const fixed = fixedLabels[role];
  if (fixed) return fixed;
  const words = role.replaceAll("_", " ");
  return words.charAt(0).toUpperCase() + words.slice(1);
}

export function roleListLabel(roles: readonly MatchedRole[]): string {
  return roles.map(roleChipLabel).join(", ");
}
