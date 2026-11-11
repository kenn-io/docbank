import { afterEach, expect, it, vi } from "vite-plus/test";
import { daemonProof } from "./upload-test-fixtures.js";
import { UploadReconnect, type UploadConnectionState } from "./uploadReconnect.js";

const sockets: Socket[] = [];
let proofValid = true;
let rejectConnections = false;
class Socket extends EventTarget {
  static OPEN = 1;
  readyState = 1;
  binaryType = "";
  constructor(_url: string) {
    super();
    sockets.push(this);
    queueMicrotask(() => this.dispatchEvent(new Event(rejectConnections ? "close" : "open")));
  }
  send(data: string): void {
    const message = JSON.parse(data) as { type: string; nonce: string };
    if (message.type !== "authenticate") throw new Error("Unexpected upload frame");
    const proof = daemonProof("session", message.nonce);
    queueMicrotask(() =>
      this.dispatchEvent(
        new MessageEvent("message", {
          data: JSON.stringify({ type: "authenticated", proof: proofValid ? proof : "bad" }),
        }),
      ),
    );
  }
  close(): void {
    this.readyState = 3;
  }
  disconnect(): void {
    this.readyState = 3;
    this.dispatchEvent(new Event("close"));
  }
}
const session = { token: "session", uploadSecret: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE" };

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  sockets.length = 0;
  proofValid = true;
  rejectConnections = false;
});

it("reconnects a dropped channel without sending upload frames", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("WebSocket", Socket);
  const states: UploadConnectionState[] = [];
  const connection = new UploadReconnect(session, (state) => states.push(state));
  connection.start();
  await vi.waitFor(() => expect(states).toEqual(["connected"]));
  sockets[0].disconnect();
  await vi.advanceTimersByTimeAsync(250);
  expect(states).toEqual(["connected", "reconnecting", "connected"]);
  expect(sockets).toHaveLength(2);
  connection.close();
});

it.each([401, 403])(
  "offers a manual retry and stops for a rejected session: %s",
  async (status) => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", Socket);
    const fetcher = vi.fn().mockResolvedValue(new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetcher);
    const states: UploadConnectionState[] = [];
    const connection = new UploadReconnect(session, (state) => states.push(state));
    connection.start();
    await vi.waitFor(() => expect(states).toEqual(["connected"]));
    rejectConnections = true;
    sockets[0].disconnect();
    await vi.advanceTimersByTimeAsync(4000);
    expect(states.at(-1)).toBe("failed");
    fetcher.mockResolvedValue(new Response("{}", { status }));
    connection.retry();
    await vi.advanceTimersByTimeAsync(250);
    expect(states.slice(-2)).toEqual(["manual", "fatal"]);
    connection.close();
  },
);

it("disposes a pending retry without opening another connection", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("WebSocket", Socket);
  const states: UploadConnectionState[] = [];
  const connection = new UploadReconnect(session, (state) => states.push(state));
  connection.start();
  await vi.waitFor(() => expect(states).toEqual(["connected"]));
  sockets[0].disconnect();
  connection.close();
  await vi.advanceTimersByTimeAsync(4000);
  expect(sockets).toHaveLength(1);
  expect(states).toEqual(["connected", "reconnecting"]);
});

it("stops when the new daemon proof is wrong", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("WebSocket", Socket);
  const states: UploadConnectionState[] = [];
  const connection = new UploadReconnect(session, (state) => states.push(state));
  connection.start();
  await vi.waitFor(() => expect(states).toEqual(["connected"]));
  proofValid = false;
  sockets[0].disconnect();
  await vi.advanceTimersByTimeAsync(250);
  expect(states).toEqual(["connected", "reconnecting", "fatal"]);
  connection.close();
});
