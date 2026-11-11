import { afterEach, expect, it, vi } from "vite-plus/test";
import { DaemonSocket } from "./upload-test-fixtures.js";
import { UploadReconnect, type UploadConnectionState } from "./uploadReconnect.js";

const sockets: Socket[] = [];
let proofValid = true;
let rejectConnections = false;
class Socket extends DaemonSocket {
  constructor() {
    super();
    this.token = "session";
    this.proofValid = proofValid;
    this.rejectConnection = rejectConnections;
    sockets.push(this);
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
  sockets[0].close();
  await vi.advanceTimersByTimeAsync(250);
  expect(states).toEqual(["connected", "reconnecting", "connected"]);
  expect(sockets).toHaveLength(2);
  expect(
    sockets.flatMap((socket) => socket.frames).map((frame) => JSON.parse(frame as string).type),
  ).toEqual(["authenticate", "authenticate"]);
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
    sockets[0].close();
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
  sockets[0].close();
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
  sockets[0].close();
  await vi.advanceTimersByTimeAsync(250);
  expect(states).toEqual(["connected", "reconnecting", "fatal"]);
  connection.close();
});
