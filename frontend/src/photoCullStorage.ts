import { idbRequest, transactionDone } from "./indexedDB.js";

export type CullScope = "unrated" | "picks" | "rejects" | "all";
export interface CullCheckpoint {
  runID: string;
  pass: string[];
  position: number;
  visited: string[];
  skipped: string[];
  unavailable: string[];
  scope: CullScope;
  autoAdvance: boolean;
}
export interface CullRun {
  ids: string[];
  checkpoint: CullCheckpoint;
}
type MarkerKind = "visited" | "skipped" | "unavailable";
type Header = Pick<CullCheckpoint, "runID" | "position" | "scope" | "autoAdvance">;
export interface CullProgress {
  runID: string;
  position?: number;
  autoAdvance?: boolean;
  marker?: { kind: MarkerKind; id: string; index: number };
  pass?: CullCheckpoint;
}
export interface CullStorage {
  load(): Promise<CullRun | undefined>;
  replace(run: CullRun, signal?: AbortSignal): Promise<void>;
  checkpoint(value: CullProgress): Promise<void>;
}

const stores = ["members", "checkpoints", "markers"];
const markerKinds: MarkerKind[] = ["visited", "skipped", "unavailable"];
const header = ({ runID, position, scope, autoAdvance }: CullCheckpoint): Header => ({
  runID,
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
  async function savePass(tx: IDBTransaction, checkpoint: CullCheckpoint) {
    const markers = tx.objectStore("markers");
    const keys = await idbRequest(markers.getAllKeys(markerRange()));
    for (const key of keys) markers.delete(key);
    for (const kind of markerKinds)
      checkpoint[kind].forEach((id, index) => markers.put(id, [vaultID, kind, index]));
    tx.objectStore("checkpoints").put(header(checkpoint), vaultID);
  }
  return {
    async load() {
      const db = await open();
      try {
        const tx = db.transaction(stores, "readonly"),
          done = transactionDone(tx);
        const [membership, saved, keys, values] = await Promise.all([
          idbRequest<{ ids: string[]; pass: string[] } | undefined>(
            tx.objectStore("members").get(vaultID),
          ),
          idbRequest<Header | undefined>(tx.objectStore("checkpoints").get(vaultID)),
          idbRequest(tx.objectStore("markers").getAllKeys(markerRange())),
          idbRequest<string[]>(tx.objectStore("markers").getAll(markerRange())),
        ]);
        await done;
        if (!membership || !saved) return;
        const checkpoint: CullCheckpoint = {
          ...saved,
          pass: membership.pass,
          visited: [],
          skipped: [],
          unavailable: [],
        };
        keys.forEach((key, index) =>
          checkpoint[(key as [string, MarkerKind, number])[1]].push(values[index]!),
        );
        return { ids: membership.ids, checkpoint };
      } finally {
        db.close();
      }
    },
    async replace(run, signal) {
      const db = await open();
      try {
        const tx = db.transaction(stores, "readwrite"),
          done = transactionDone(tx);
        const cancel = () => tx.abort();
        signal?.addEventListener("abort", cancel, { once: true });
        try {
          signal?.throwIfAborted();
          tx.objectStore("members").put({ ids: run.ids, pass: run.checkpoint.pass }, vaultID);
          await savePass(tx, run.checkpoint);
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
        const current = await idbRequest<Header | undefined>(store.get(vaultID));
        if (current?.runID !== value.runID) {
          tx.abort();
          await done.catch(() => {});
          throw new Error("Another cull run replaced this run. Close and resume it.");
        }
        if (value.pass) {
          const members = tx.objectStore("members");
          const membership = await idbRequest<{ ids: string[]; pass: string[] }>(
            members.get(vaultID),
          );
          members.put({ ...membership, pass: value.pass.pass }, vaultID);
          await savePass(tx, value.pass);
        } else {
          if (value.position !== undefined) current.position = value.position;
          if (value.autoAdvance !== undefined) current.autoAdvance = value.autoAdvance;
          store.put(current, vaultID);
          if (value.marker) {
            const { kind, id, index } = value.marker;
            tx.objectStore("markers").put(id, [vaultID, kind, index]);
          }
        }
        await done;
      } finally {
        db.close();
      }
    },
  };
}
