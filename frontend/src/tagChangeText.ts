export function documentCount(count: number): string {
  return `${count.toLocaleString()} document${count === 1 ? "" : "s"}`;
}

/** One line that says what Apply changes and whether it can be undone. */
export function tagChangeConsequence(assign: boolean, tagName: string, count: number): string {
  return assign
    ? `Adds “${tagName}” to ${documentCount(count)}. Undo with Remove.`
    : `Removes “${tagName}” from ${documentCount(count)}. This can't be undone.`;
}

export function tagChangeDone(assign: boolean, tagName: string, changed: number, total: number) {
  if (changed === 0)
    return assign
      ? `All ${documentCount(total)} already had “${tagName}”.`
      : `None of the ${documentCount(total)} had “${tagName}”.`;
  const done = assign
    ? `Added “${tagName}” to ${documentCount(changed)}.`
    : `Removed “${tagName}” from ${documentCount(changed)}.`;
  const unchanged = total - changed;
  if (unchanged <= 0) return done;
  return `${done} ${unchanged.toLocaleString()} already ${assign ? "had" : "lacked"} it.`;
}
