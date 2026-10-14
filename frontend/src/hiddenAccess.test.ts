import { afterEach, expect, it, vi } from "vitest";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { unlockPhotoHidden } from "./generated/docbank.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it("pagehide sends a keepalive lock without waiting for a held unlock", async () => {
  let finish!: (response: Response) => void;
  const fetcher = vi.fn((url: string) => url.endsWith("/unlock") ? new Promise<Response>(resolve => finish = resolve) : Promise.resolve(Response.json({ configured: true })));
  vi.stubGlobal("fetch", fetcher);
  const access = new HiddenAccess("synthetic", vi.fn());
  const unlock = access.run(() => unlockPhotoHidden({ passcode: "synthetic" }, { session: "synthetic" }));
  await vi.waitFor(() => expect(finish).toBeTypeOf("function"));
  access.pagehide();
  const lock = access.lock({ keepalive: true });
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(vi.mocked(fetch).mock.calls[1][1]).toMatchObject({ keepalive: true });
  finish(Response.json({ configured: true, expires_in: 300 }));
  await unlock;
  await lock;
  expect(fetcher.mock.calls.map(([url]) => url.split("/").at(-1))).toEqual(["unlock", "lock", "lock"]);
});

it("locks after a held unlock and starts its timeout when dispatched", async () => {
  let finish!: (response: Response) => void;
  const timeout = vi.spyOn(AbortSignal, "timeout");
  const fetcher = vi.fn((url: string) => url.endsWith("/unlock") ? new Promise<Response>(resolve => finish = resolve) : Promise.resolve(Response.json({ configured: true })));
  vi.stubGlobal("fetch", fetcher);
  const access = new HiddenAccess("synthetic", vi.fn());
  const unlock = access.run(() => unlockPhotoHidden({ passcode: "synthetic" }, { session: "synthetic" }));
  await vi.waitFor(() => expect(finish).toBeTypeOf("function"));
  const lock = access.lock({ keepalive: true });
  let settled = false;
  void access.settled().then(() => settled = true);
  await Promise.resolve();
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(timeout).not.toHaveBeenCalled();
  expect(settled).toBe(false);
  finish(Response.json({ configured: true, expires_in: 300 }));
  await unlock;
  expect(await lock).toBe(true);
  await access.settled();
  expect(fetcher.mock.calls.map(([url]) => url.split("/").at(-1))).toEqual(["unlock", "lock"]);
  expect(vi.mocked(fetch).mock.calls[1][1]).toMatchObject({ keepalive: true });
  expect(timeout).toHaveBeenCalledWith(30_000);
  expect(settled).toBe(true);
});

it("continues after a rejected request", async () => {
  const access = new HiddenAccess("synthetic", vi.fn());
  const failed = access.run(async () => { throw new Error("Unlock unavailable"); });
  const next = access.run(async () => "next");
  await expect(failed).rejects.toThrow("Unlock unavailable");
  expect(await next).toBe("next");
});

it("reports a failed lock and clears the error after retry", async () => {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValueOnce(new Error("Lock unavailable")).mockResolvedValueOnce(Response.json({ configured: true })));
  const access = new HiddenAccess("synthetic", vi.fn());
  expect(await access.lock()).toBe(false);
  expect(access.lockError).toBe("Lock unavailable");
  expect(await access.lock()).toBe(true);
  expect(access.lockError).toBe("");
});

it("reports an expired browser session to the auth handler", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => Response.json({ detail: "Session expired" }, { status: 401 })));
  const onauthfailure = vi.fn();
  const access = new HiddenAccess("synthetic", onauthfailure);
  expect(await access.lock()).toBe(false);
  expect(onauthfailure).toHaveBeenCalledWith(expect.objectContaining({ status: 401 }));
});
