import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import TagPicker from "./TagPicker.svelte";

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

const tags = [
  {
    id: "22222222-2222-4222-8222-222222222222",
    name: "matter/zeta/hold",
    revision: 1,
    assignment_count: 2,
  },
  {
    id: "11111111-1111-4111-8111-111111111111",
    name: "matter/acme/reviewed",
    revision: 1,
    assignment_count: 7,
  },
  {
    id: "33333333-3333-4333-8333-333333333333",
    name: "important",
    revision: 1,
    assignment_count: 4,
  },
];

it("shows grouped leaf labels with full accessible names and identity colors", async () => {
  const onchange = vi.fn();
  const rendered = render(TagPicker, {
    value: "",
    tags,
    title: "Tag to assign",
    placeholder: "Choose a tag…",
    onchange,
  });

  const trigger = screen.getByRole("combobox", {
    name: "Tag to assign: Choose a tag…",
  });
  await fireEvent.click(trigger);

  expect(screen.getByText("matter/acme")).toBeTruthy();
  expect(screen.getByText("matter/zeta")).toBeTruthy();
  const reviewed = screen.getByRole("option", {
    name: "matter/acme/reviewed (7)",
  });
  const hold = screen.getByRole("option", {
    name: "matter/zeta/hold (2)",
  });
  expect(reviewed.textContent).toContain("reviewed");
  expect(reviewed.textContent).not.toContain("matter/acme/reviewed");
  expect(reviewed.querySelector<HTMLElement>("[data-tag-swatch]")?.style.backgroundColor).toBe(
    "rgb(188, 76, 0)",
  );
  expect(hold.querySelector<HTMLElement>("[data-tag-swatch]")?.style.backgroundColor).toBe(
    "rgb(87, 96, 106)",
  );

  await fireEvent.click(reviewed);
  expect(onchange).toHaveBeenCalledWith(tags[1]?.id);
  await rendered.rerender({
    value: tags[1]!.id,
    tags,
    title: "Tag to assign",
    placeholder: "Choose a tag…",
    onchange,
  });
  expect(trigger.getAttribute("aria-label")).toBe("Tag to assign: matter/acme/reviewed");
  expect(document.activeElement).toBe(trigger);
});

it("selects by arrows, Home, End, and Enter and restores focus on Escape", async () => {
  const onchange = vi.fn();
  render(TagPicker, {
    value: "",
    tags,
    title: "Browse or filter by tag",
    placeholder: "All tags",
    includeAll: true,
    onchange,
  });

  const trigger = screen.getByRole("combobox", {
    name: "Browse or filter by tag: All tags",
  });
  trigger.focus();
  await fireEvent.keyDown(trigger, { key: "ArrowDown" });
  expect(trigger.getAttribute("aria-expanded")).toBe("true");
  await fireEvent.keyDown(trigger, { key: "End" });
  expect(trigger.getAttribute("aria-activedescendant")).toBe(
    screen.getByRole("option", { name: "matter/zeta/hold (2)" }).id,
  );
  await fireEvent.keyDown(trigger, { key: "Home" });
  expect(trigger.getAttribute("aria-activedescendant")).toBe(
    screen.getByRole("option", { name: "All tags" }).id,
  );
  await fireEvent.keyDown(trigger, { key: "ArrowUp" });
  await fireEvent.keyDown(trigger, { key: "Enter" });
  expect(onchange).toHaveBeenCalledWith(tags[0]?.id);
  expect(trigger.getAttribute("aria-expanded")).toBe("false");

  await fireEvent.keyDown(trigger, { key: "ArrowDown" });
  await fireEvent.keyDown(document, { key: "Escape" });
  expect(trigger.getAttribute("aria-expanded")).toBe("false");
  expect(document.activeElement).toBe(trigger);

  await fireEvent.keyDown(trigger, { key: "ArrowDown" });
  await fireEvent.mouseDown(document.body);
  expect(trigger.getAttribute("aria-expanded")).toBe("false");
});

it("closes on Tab without preventing the browser focus move", async () => {
  render(TagPicker, {
    value: "",
    tags,
    title: "Tag to assign",
    placeholder: "Choose a tag…",
    onchange: vi.fn(),
  });
  const trigger = screen.getByRole("combobox", {
    name: "Tag to assign: Choose a tag…",
  });
  await fireEvent.keyDown(trigger, { key: "ArrowDown" });
  const notCanceled = await fireEvent.keyDown(trigger, { key: "Tab" });

  expect(notCanceled).toBe(true);
  expect(trigger.getAttribute("aria-expanded")).toBe("false");
});

it("does not open or select while disabled", async () => {
  const onchange = vi.fn();
  render(TagPicker, {
    value: "",
    tags,
    title: "Tag to assign",
    placeholder: "Choose a tag…",
    disabled: true,
    onchange,
  });
  const trigger = screen.getByRole("combobox", {
    name: "Tag to assign: Choose a tag…",
  });

  expect(trigger.hasAttribute("disabled")).toBe(true);
  await fireEvent.click(trigger);
  await fireEvent.keyDown(trigger, { key: "ArrowDown" });
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(onchange).not.toHaveBeenCalled();
});

it("opens tag management from an empty tag filter with the keyboard", async () => {
  const onmanage = vi.fn();
  const onchange = vi.fn();
  render(TagPicker, {
    value: "",
    tags: [],
    title: "Filter by tag",
    placeholder: "All tags",
    includeAll: true,
    onchange,
    onmanage,
  });
  const trigger = screen.getByRole("combobox");
  trigger.focus();
  await fireEvent.keyDown(trigger, { key: "ArrowDown" });
  await fireEvent.keyDown(trigger, { key: "Tab" });
  const manage = screen.getByRole("button", { name: "Manage tags" });
  expect(document.activeElement).toBe(manage);
  await fireEvent.click(manage);
  expect(onmanage).toHaveBeenCalledOnce();
  expect(onchange).not.toHaveBeenCalled();
  expect(document.activeElement).toBe(trigger);
});

it("filters existing tags and selects the highlighted match with Enter", async () => {
  const onpick = vi.fn();
  render(TagPicker, {
    value: "",
    tags,
    title: "Tag to assign",
    placeholder: "Tag name",
    session: "session",
    onchange: vi.fn(),
    onpick,
  });
  const input = screen.getByRole("combobox", { name: "Tag to assign" });
  await fireEvent.input(input, { target: { value: "matter" } });
  expect(screen.queryByRole("option", { name: "important (4)" })).toBeNull();
  await fireEvent.keyDown(input, { key: "ArrowDown" });
  await fireEvent.keyDown(input, { key: "Enter" });
  expect(onpick).toHaveBeenCalledWith(tags[0], false);
});

it("creates from an empty catalog, then hands the new tag to assignment", async () => {
  const tag = { ...tags[2], name: "Review", revision: 1, assignment_count: 0 };
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json(tag));
  const onpick = vi.fn();
  render(TagPicker, {
    value: "",
    tags: [],
    title: "Tag to assign",
    placeholder: "Tag name",
    session: "session",
    onchange: vi.fn(),
    onpick,
  });
  const input = screen.getByRole("combobox", { name: "Tag to assign" });
  expect(input.hasAttribute("disabled")).toBe(false);
  await fireEvent.input(input, { target: { value: "Review" } });
  await fireEvent.keyDown(input, { key: "Enter" });
  await vi.waitFor(() => expect(onpick).toHaveBeenCalledWith(tag, true));
  expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/tags");
  expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toEqual({ name: "Review" });
  vi.restoreAllMocks();
});

it("doesn't offer creation for a normalized existing assigned tag", async () => {
  render(TagPicker, {
    value: "",
    tags,
    title: "Tag to assign",
    placeholder: "Tag name",
    session: "session",
    excludedTagIDs: [tags[2]!.id],
    onchange: vi.fn(),
    onpick: vi.fn(),
  });
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "important" } });
  expect(screen.queryByRole("option")).toBeNull();
});

it("keeps creation errors visible and lets the name be corrected", async () => {
  const fetchMock = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(Response.json({ detail: "Tag name is too long" }, { status: 400 }));
  const onpick = vi.fn();
  render(TagPicker, {
    value: "",
    tags: [],
    title: "Tag to assign",
    placeholder: "Tag name",
    session: "session",
    onchange: vi.fn(),
    onpick,
  });
  const input = screen.getByRole("combobox");
  await fireEvent.input(input, { target: { value: "Invalid" } });
  await fireEvent.keyDown(input, { key: "Enter" });
  expect((await screen.findByRole("alert")).textContent).toBe("Tag name is too long");
  expect(onpick).not.toHaveBeenCalled();
  await fireEvent.input(input, { target: { value: "Review" } });
  expect(screen.queryByRole("alert")).toBeNull();
  expect(screen.getByRole("option", { name: "Create “Review”" })).toBeTruthy();
  expect(fetchMock).toHaveBeenCalledOnce();
  vi.restoreAllMocks();
});

it("can stay closed after a pick instead of reopening the list", async () => {
  for (const reopenAfterPick of [true, false]) {
    const picked: string[] = [];
    const view = render(TagPicker, {
      value: "",
      tags,
      title: "Tag to assign",
      placeholder: "Tag name",
      session: "session",
      reopenAfterPick,
      onchange: vi.fn(),
      onpick: async (tag) => {
        // A pointer pick moves focus to the option before the picker restores it.
        (document.activeElement as HTMLElement | null)?.blur();
        picked.push(tag.id);
        await view.rerender({ value: tag.id });
      },
    });
    const input = screen.getByRole("combobox", { name: "Tag to assign" }) as HTMLInputElement;
    input.focus();
    await fireEvent.input(input, { target: { value: "matter" } });
    await fireEvent.keyDown(input, { key: "Enter" });
    await vi.waitFor(() => expect(document.activeElement).toBe(input));
    expect(picked).toHaveLength(1);
    expect(screen.queryAllByRole("option").length > 0).toBe(reopenAfterPick);
    view.unmount();
  }
});
