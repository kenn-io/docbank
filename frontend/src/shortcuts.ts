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
  allowedTargetSelector?: string,
): boolean {
  if (event.defaultPrevented || event.isComposing || unavailable) return true;
  if (root.querySelector(OPEN_DIALOGS)) return true;
  const target = event.target;
  return (
    target instanceof Element &&
    target.closest(NATIVE_KEYBOARD_TARGETS) !== null &&
    !(allowedTargetSelector && target.closest(allowedTargetSelector))
  );
}
export type InspectionBoundary = "first" | "last" | "empty";

export function isPhotoShortcutSuppressed(
  event: KeyboardEvent,
  root: ParentNode = document,
): boolean {
  if (event.defaultPrevented || event.repeat || event.isComposing) return true;
  if (
    [...root.querySelectorAll(OPEN_DIALOGS)].some(
      (dialog) => !dialog.classList.contains("photo-loupe"),
    )
  )
    return true;
  const target = event.target;
  if (!(target instanceof Element)) return false;
  if (
    target.closest(
      "input:not([type=checkbox]),textarea,select,[contenteditable]:not([contenteditable=false]),[role=combobox],[role=listbox],[role=menu],[role=menuitem]",
    )
  )
    return true;
  const control = target.closest(NATIVE_KEYBOARD_TARGETS);
  return (
    !!control &&
    !(
      control.matches("button,input[type=checkbox],[role=button],[role=checkbox]") &&
      control.closest(".photo-cell,.photo-loupe,.photo-inspector")
    )
  );
}

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

export function cullKey(event: KeyboardEvent): string | number | undefined {
  if (event.ctrlKey || event.metaKey || event.altKey || event.repeat || event.isComposing) return;
  const target = event.target;
  if (target instanceof Element && target.closest("input,textarea,select,[contenteditable=true]"))
    return;
  const key = event.key.toLowerCase();
  if (/^[0-5]$/.test(key)) return Number(key);
  return (
    {
      arrowleft: "previous",
      arrowup: "previous",
      arrowright: "next",
      arrowdown: "next",
      p: "pick",
      x: "reject",
      u: "unflag",
      a: "auto",
      s: "scope",
      z: "zoom",
      i: "info",
      "?": "help",
      escape: "close",
    } as Record<string, string>
  )[key];
}
