import { idbRequest, transactionDone } from "./indexedDB.js";

export type CullScope = "unflagged" | "picks" | "rejects" | "all";
export interface CullCheckpoint {
  runID: string;
  revision: number;
  pass: string[];
  position: number;
  visited: string[];
  skipped: string[];
  unavailable: string[];
  scope: CullScope;
  autoAdvance: boolean;
}
export interface CullRun {
  stackEligible?: boolean;
  ids: string[];
  checkpoint: CullCheckpoint;
}
type Membership = Omit<CullRun, "checkpoint"> & { pass: string[] };
type MarkerKind = "visited" | "skipped" | "unavailable";
type Header = Pick<CullCheckpoint, "runID" | "revision" | "position" | "scope" | "autoAdvance"> & {
  formatVersion?: number;
};
const formatVersion = 1;
type StoredHeader = Header & { formatVersion: number };
export class CullFormatError extends Error {
  constructor(readonly saved: StoredHeader) {
    super("Saved cull progress uses an unsupported format. Update Docbank to resume it.");
  }
}
function checkFormat(saved: StoredHeader | undefined) {
  if (saved && saved.formatVersion !== formatVersion) throw new CullFormatError(saved);
}
export interface CullProgress {
  runID: string;
  revision: number;
  position?: number;
  autoAdvance?: boolean;
  marker?: { kind: MarkerKind; ids: string[]; index: number };
  pass?: CullCheckpoint;
}
export interface CullStorage {
  saved(): Promise<Header | undefined>;
  load(): Promise<CullRun | undefined>;
  replace(run: CullRun, signal?: AbortSignal, expected?: Header): Promise<void>;
  checkpoint(value: CullProgress): Promise<void>;
}

export const sameCheckpoint = (
  a: Pick<CullCheckpoint, "runID" | "revision">,
  b: Pick<CullCheckpoint, "runID" | "revision">,
) => a.runID === b.runID && a.revision === b.revision;

const stores = ["members", "checkpoints", "markers"];
const markerKinds: MarkerKind[] = ["visited", "skipped", "unavailable"];
const header = ({
  runID,
  revision,
  position,
  scope,
  autoAdvance,
}: CullCheckpoint): StoredHeader => ({
  formatVersion,
  runID,
  revision,
  position,
  scope,
  autoAdvance,
});

// Full membership/pass writes happen only at capture and pass boundaries.
export function photoCullStorage(vaultID: string): CullStorage {
  const markerRange = () => IDBKeyRange.bound([vaultID], [vaultID, []]);
  async function open() {
    if (!vaultID) throw new Error("The vault identity is unavailable. Reload before culling.");
    const opening = indexedDB.open("docbank-photo-cull-v1", 1);
    opening.onupgradeneeded = () => {
      for (const name of stores) opening.result.createObjectStore(name);
    };
    return idbRequest(opening);
  }
  function savePass(tx: IDBTransaction, checkpoint: CullCheckpoint) {
    const markers = tx.objectStore("markers");
    markers.delete(markerRange());
    for (const kind of markerKinds)
      checkpoint[kind].forEach((id, index) => markers.put(id, [vaultID, kind, index]));
    tx.objectStore("checkpoints").put(header(checkpoint), vaultID);
  }
  return {
    async saved() {
      const db = await open();
      try {
        const tx = db.transaction("checkpoints", "readonly"),
          done = transactionDone(tx);
        const saved = await idbRequest<StoredHeader | undefined>(
          tx.objectStore("checkpoints").get(vaultID),
        );
        await done;
        checkFormat(saved);
        return saved;
      } finally {
        db.close();
      }
    },
    async load() {
      const db = await open();
      try {
        const tx = db.transaction(stores, "readonly"),
          done = transactionDone(tx);
        const [membership, saved, keys, values] = await Promise.all([
          idbRequest<Membership | undefined>(tx.objectStore("members").get(vaultID)),
          idbRequest<StoredHeader | undefined>(tx.objectStore("checkpoints").get(vaultID)),
          idbRequest(tx.objectStore("markers").getAllKeys(markerRange())),
          idbRequest<string[]>(tx.objectStore("markers").getAll(markerRange())),
        ]);
        await done;
        checkFormat(saved);
        if (!membership || !saved) return;
        const { formatVersion: _version, ...progress } = saved;
        const checkpoint: CullCheckpoint = {
          ...progress,
          pass: membership.pass,
          visited: [],
          skipped: [],
          unavailable: [],
        };
        keys.forEach((key, index) =>
          checkpoint[(key as [string, MarkerKind, number])[1]].push(values[index]!),
        );
        return { ids: membership.ids, stackEligible: membership.stackEligible, checkpoint };
      } finally {
        db.close();
      }
    },
    async replace(run, signal, expected) {
      const db = await open();
      try {
        const tx = db.transaction(stores, "readwrite"),
          done = transactionDone(tx);
        const cancel = () => tx.abort();
        signal?.addEventListener("abort", cancel, { once: true });
        try {
          signal?.throwIfAborted();
          const current = await idbRequest<StoredHeader | undefined>(
            tx.objectStore("checkpoints").get(vaultID),
          );
          if (
            current
              ? !expected ||
                current.formatVersion !== (expected.formatVersion ?? formatVersion) ||
                !sameCheckpoint(current, expected)
              : !!expected
          ) {
            tx.abort();
            await done.catch(() => {});
            throw new Error("Cull progress changed in another tab. Resume before replacing it.");
          }
          tx.objectStore("members").put(
            { ids: run.ids, stackEligible: run.stackEligible, pass: run.checkpoint.pass },
            vaultID,
          );
          savePass(tx, run.checkpoint);
          await done;
        } finally {
          signal?.removeEventListener("abort", cancel);
        }
      } finally {
        db.close();
      }
    },
    async checkpoint(value) {
      const db = await open();
      try {
        const tx = db.transaction(value.pass ? stores : ["checkpoints", "markers"], "readwrite"),
          done = transactionDone(tx);
        const store = tx.objectStore("checkpoints");
        const current = await idbRequest<StoredHeader | undefined>(store.get(vaultID));
        if (
          !current ||
          current.formatVersion !== formatVersion ||
          !sameCheckpoint(current, value)
        ) {
          tx.abort();
          await done.catch(() => {});
          throw new Error("Cull progress changed in another tab. Close and resume it.");
        }
        if (value.pass) {
          const members = tx.objectStore("members");
          const membership = await idbRequest<Membership>(members.get(vaultID));
          members.put({ ...membership, pass: value.pass.pass }, vaultID);
          savePass(tx, { ...value.pass, revision: current.revision + 1 });
        } else {
          if (value.position !== undefined) current.position = value.position;
          if (value.autoAdvance !== undefined) current.autoAdvance = value.autoAdvance;
          current.revision++;
          store.put(current, vaultID);
          if (value.marker) {
            const { kind, ids, index } = value.marker;
            ids.forEach((id, offset) =>
              tx.objectStore("markers").put(id, [vaultID, kind, index + offset]),
            );
          }
        }
        await done;
      } finally {
        db.close();
      }
    },
  };
}
