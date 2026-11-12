// Tax amounts stay in the wire's integer minor units (spec D65): the digits
// are shown exactly, with no exponent lookup or locale formatting.
export function taxStateLabel(state: string): string {
  const words = state.replaceAll("_", " ");
  return words.charAt(0).toUpperCase() + words.slice(1);
}

export function formatTaxAmount(
  minor: number | null,
  currency: string,
  direction: string,
): { text: string; direction: string; hint?: string } {
  const label = taxStateLabel(direction);
  if (minor === null) return { text: "Unknown", direction: label };
  if (Number.isSafeInteger(minor) && minor >= 0)
    return { text: `${minor} ${currency}`, direction: label };
  // response.json() has already rounded these, so no digits are honest.
  if (Number.isInteger(minor) && minor > Number.MAX_SAFE_INTEGER)
    return {
      text: "Too large to show",
      direction: label,
      hint: "Use docbank tax record show to see the exact amount",
    };
  return { text: "Invalid amount", direction: label };
}
