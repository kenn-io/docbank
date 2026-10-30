import { afterEach, expect, it } from "vite-plus/test";
import { cleanup, render, screen } from "@testing-library/svelte";
import MarkdownPreview from "./MarkdownPreview.svelte";

afterEach(cleanup);

it("renders document structure and keeps embedded active HTML inert", async () => {
  render(MarkdownPreview, {
    text: '# Project notes\n\n**Ready**\n\n- First item\n\n| Name | State |\n| --- | --- |\n| Draft | Done |\n\n<script>window.markdownExecuted=true</script><img src=x onerror="window.markdownExecuted=true">\n\n[Bad](javascript:alert(1))',
  });
  expect(await screen.findByRole("heading", { name: "Project notes" })).toBeTruthy();
  expect(screen.getByRole("listitem").textContent).toBe("First item");
  expect(screen.getByRole("cell", { name: "Done" })).toBeTruthy();
  expect(document.querySelector("script, [onerror], a[href^='javascript:']")).toBeNull();
});
