type ToastAction = { label: string; run: () => void | Promise<void> };
type Toast = {
  id: number;
  message: string;
  tone: "neutral" | "danger";
  action?: ToastAction;
};

const DEFAULT_MS = 8000;
const ERROR_MS = 12000;
const MAX_TOASTS = 3;

let toasts = $state<Toast[]>([]);
const timers = new Map<number, ReturnType<typeof setTimeout>>();
type Interaction = "pointer" | "focus";
const holds = new Map<number, Set<Interaction>>();
let nextID = 0;

export function getToasts(): Toast[] {
  return toasts;
}

export function showToast(
  message: string,
  options: { action?: ToastAction; tone?: Toast["tone"] } = {},
): number {
  const tone = options.tone ?? "neutral";
  const id = ++nextID;
  while (toasts.length >= MAX_TOASTS) dismissToast(toasts[0]!.id);
  toasts = [...toasts, { id, message, tone, action: options.action }];
  timers.set(
    id,
    setTimeout(() => dismissToast(id), tone === "danger" ? ERROR_MS : DEFAULT_MS),
  );
  return id;
}

export function dismissToast(id: number): void {
  const timer = timers.get(id);
  if (timer !== undefined) clearTimeout(timer);
  timers.delete(id);
  holds.delete(id);
  toasts = toasts.filter((toast) => toast.id !== id);
}

// Hovering or focusing a toast holds it open so the action stays reachable.
export function holdToast(id: number, interaction: Interaction): void {
  const active = holds.get(id) ?? new Set<Interaction>();
  active.add(interaction);
  holds.set(id, active);
  const timer = timers.get(id);
  if (timer !== undefined) clearTimeout(timer);
  timers.delete(id);
}

export function releaseToast(id: number, interaction: Interaction): void {
  const active = holds.get(id);
  active?.delete(interaction);
  if (active?.size) return;
  holds.delete(id);
  if (!toasts.some((toast) => toast.id === id) || timers.has(id)) return;
  timers.set(
    id,
    setTimeout(() => dismissToast(id), DEFAULT_MS / 2),
  );
}

export function clearToasts(): void {
  for (const id of toasts.map((toast) => toast.id)) dismissToast(id);
}
