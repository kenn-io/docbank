import { afterEach, expect, it, vi } from "vitest";
import { signIn } from "./web-login.js";

afterEach(() => { vi.restoreAllMocks(); });

it("exchanges a key without forwarding it as a vault header or following redirects", async () => {
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ enabled: true })));
  await signIn("synthetic-key");
  const options = fetchMock.mock.calls[0]?.[1];
  expect(options?.redirect).toBe("error");
  expect(options?.credentials).toBe("same-origin");
  expect(new Headers(options?.headers).has("X-Api-Key")).toBe(false);
  expect(JSON.parse(String(options?.body))).toEqual({ api_key: "synthetic-key" });
});
