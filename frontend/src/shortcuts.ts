const NATIVE_KEYBOARD_ROLES = [
  "button",
  "link",
  "checkbox",
  "radio",
  "switch",
  "menu",
  "menuitem",
  "combobox",
  "listbox",
  "option",
  "slider",
  "spinbutton",
  "tab",
];

const NATIVE_KEYBOARD_TARGETS = [
  "input",
  "textarea",
  "select",
  "button",
  "a[href]",
  "summary",
  '[contenteditable]:not([contenteditable="false"])',
  ...NATIVE_KEYBOARD_ROLES.map((role) => `[role=${role}]`),
].join(",");

const OPEN_DIALOGS = "[role=dialog][aria-modal=true], dialog[open]";

export function isAppShortcutSuppressed(
  event: KeyboardEvent,
  unavailable: boolean,
  root: ParentNode = document,
): boolean {
  if (event.defaultPrevented || event.isComposing || unavailable) return true;
  if (root.querySelector(OPEN_DIALOGS)) return true;
  const target = event.target;
  return target instanceof Element && target.closest(NATIVE_KEYBOARD_TARGETS) !== null;
}

export type InspectionBoundary = "first" | "last" | "empty";

export interface InspectionMove {
  id?: number;
  boundary?: InspectionBoundary;
}

export function moveInspection<T extends { id: number }>(
  displayed: readonly T[],
  inspectedID: number | undefined,
  direction: -1 | 1,
): InspectionMove {
  if (displayed.length === 0) return { boundary: "empty" };
  const current = displayed.findIndex((item) => item.id === inspectedID);
  if (current < 0) {
    return { id: direction > 0 ? displayed[0]!.id : displayed.at(-1)!.id };
  }
  const next = current + direction;
  if (next < 0) return { id: displayed[0]!.id, boundary: "first" };
  if (next >= displayed.length) {
    return { id: displayed.at(-1)!.id, boundary: "last" };
  }
  return { id: displayed[next]!.id };
}
