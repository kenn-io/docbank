import { afterEach, expect, it, vi } from "vite-plus/test";
import { signIn } from "./web-login.js";

afterEach(() => {
  vi.restoreAllMocks();
});

it("sends the API key in the sign-in body", async () => {
  const fetchMock = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(new Response(JSON.stringify({ enabled: true })));
  await signIn("synthetic-key");
  const options = fetchMock.mock.calls[0]?.[1];
  expect(new Headers(options?.headers).has("X-Api-Key")).toBe(false);
  expect(JSON.parse(String(options?.body))).toEqual({ api_key: "synthetic-key" });
});
