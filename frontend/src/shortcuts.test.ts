import { afterEach, describe, expect, it } from "vitest";
import {
  isAppShortcutSuppressed,
  moveInspection,
} from "./shortcuts.js";

afterEach(() => {
  document.body.replaceChildren();
});

function keydown(target: Element, init: KeyboardEventInit = {}): KeyboardEvent {
  const event = new KeyboardEvent("keydown", {
    key: "j",
    bubbles: true,
    cancelable: true,
    ...init,
  });
  target.dispatchEvent(event);
  return event;
}

describe("application shortcut suppression", () => {
  it.each([
    ["input", "<input>"],
    ["textarea descendant", "<textarea><span></span></textarea>"],
    ["select", "<select></select>"],
    ["contenteditable descendant", '<div contenteditable="true"><span></span></div>'],
    ["button", "<button></button>"],
    ["link descendant", '<a href="/elsewhere"><span></span></a>'],
    ["checkbox role", '<div role="checkbox"><span></span></div>'],
    ["menu item", '<div role="menuitem"><span></span></div>'],
    ["combobox", '<div role="combobox"><span></span></div>'],
  ])("preserves native keyboard behavior for %s", (_name, markup) => {
    document.body.innerHTML = markup;
    const target = document.querySelector("span") ?? document.body.firstElementChild!;
    const event = keydown(target);

    expect(isAppShortcutSuppressed(event, false)).toBe(true);
  });

  it("suppresses prevented, composing, unavailable, and modal interactions", () => {
    const plain = document.body.appendChild(document.createElement("div"));

    const prevented = new KeyboardEvent("keydown", { key: "j", cancelable: true });
    prevented.preventDefault();
    expect(isAppShortcutSuppressed(prevented, false)).toBe(true);
    expect(
      isAppShortcutSuppressed(
        new KeyboardEvent("keydown", { key: "j", isComposing: true }),
        false,
      ),
    ).toBe(true);
    expect(isAppShortcutSuppressed(keydown(plain), true)).toBe(true);

    const dialog = document.body.appendChild(document.createElement("div"));
    dialog.setAttribute("role", "dialog");
    dialog.setAttribute("aria-modal", "true");
    expect(isAppShortcutSuppressed(keydown(plain), false)).toBe(true);
  });

  it("allows an unmodified shortcut from a noninteractive surface", () => {
    const surface = document.body.appendChild(document.createElement("div"));
    expect(isAppShortcutSuppressed(keydown(surface), false)).toBe(false);
  });
});

describe("loaded-page inspection", () => {
  const displayed = [{ id: 30 }, { id: 10 }, { id: 20 }];

  it("moves in displayed order without sorting IDs", () => {
    expect(moveInspection(displayed, 30, 1)).toEqual({ id: 10 });
    expect(moveInspection(displayed, 20, -1)).toEqual({ id: 10 });
  });

  it("stops and reports the first and last loaded boundaries", () => {
    expect(moveInspection(displayed, 30, -1)).toEqual({
      id: 30,
      boundary: "first",
    });
    expect(moveInspection(displayed, 20, 1)).toEqual({
      id: 20,
      boundary: "last",
    });
  });

  it("is safe for empty pages and missing inspection", () => {
    expect(moveInspection([], undefined, 1)).toEqual({ boundary: "empty" });
    expect(moveInspection(displayed, undefined, 1)).toEqual({ id: 30 });
    expect(moveInspection(displayed, 999, -1)).toEqual({ id: 20 });
  });
});
