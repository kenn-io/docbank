// OpenCode 2.x TUI plugin API:
// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/plugin/src/tui/context.ts
const routePollMilliseconds = 500;

export default {
	id: "kenn.agenthook",
	setup(api) {
		// The TUI hears every terminal's sessions, so report only the root this
		// terminal shows. Memory survives hot reloads, so a reload on the home
		// route still knows the root a running turn belongs to:
		// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/plugin/src/tui/context.ts#L43-L52
		const [state, update] = api.storage.memory("kenn.agenthook", { initial: { root: null, working: false } });
		let pending = Promise.resolve();
		const emit = (event, fields) => {
			if (event === "UserPromptSubmit" || event === "Stop" || event === "SessionStart") {
				update((draft) => {
					draft.working = event === "UserPromptSubmit";
				});
			}
			// The TUI's cwd is the directory the terminal launched it in; it never
			// changes after startup, so it matches through /cd and moved sessions:
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/cli/src/commands/handlers/default.ts#L23
			const payload = { hook_event_name: event, session_id: state.root, cwd: process.cwd(), ...fields };
			// A failed report must not block later ones; OpenCode shows plugin
			// errors as error toasts, so this one does too:
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/plugin/src/tui/context.ts#L271-L282
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/tui/src/plugin/context.tsx#L235-L238
			pending = pending
				.then(() => runHooks(event, payload))
				.catch((error) => {
					try {
						api.ui.toast.show({ variant: "error", title: "Plugin", message: `kenn.agenthook: ${error.message}` });
					} catch {}
				});
		};
		// The shown root, or null while the route's session or its root loads; a
		// loaded root record means root() walked a complete parent chain:
		// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/client/src/solid/data.ts#L501-L512
		// The home route keeps the tracked root, since a turn may finish there.
		const shown = () => {
			const route = api.ui.router.current();
			if (route.type !== "session") return state.root;
			const id = api.data.session.root(route.sessionID);
			return api.data.session.get(id) ? id : null;
		};
		// The plugin can't tell /new from opening an existing session or a
		// deleted one, so the reason is always other.
		const retire = () => {
			emit("SessionEnd", { reason: "other" });
			update((draft) => {
				draft.root = null;
			});
		};
		const sync = () => {
			const id = shown();
			if (id === null) return;
			if (id !== state.root) {
				if (state.root !== null) retire();
				update((draft) => {
					draft.root = id;
				});
				emit("SessionStart", {});
			}
			// status reads the running state that setSessionActive updates:
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/client/src/solid/data.ts#L257-L260
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/client/src/solid/data.ts#L1378-L1380
			const status = api.data.session.status(id);
			if (!state.working && status === "running") emit("UserPromptSubmit", {});
			if (state.working && status === "idle") emit("Stop", {});
		};
		const unsubscribe = api.data.listen(({ details }) => {
			// The record is already gone, so this precedes the route check:
			// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/tui/src/app.tsx#L1281-L1291
			if (details.type === "session.deleted" && details.data.sessionID === state.root) {
				retire();
				return;
			}
			sync();
		});
		// Setup re-announces a tracked root with a record, or retires a deleted root:
		// https://github.com/anomalyco/opencode/blob/v2.0.24/packages/plugin/src/tui/context.ts#L75
		if (state.root !== null) {
			if (api.data.session.get(state.root)) {
				// Reannouncement must not erase a completion missed during cleanup.
				const wasWorking = state.working;
				emit("SessionStart", {});
				if (wasWorking && api.data.session.status(state.root) === "idle") emit("Stop", {});
			} else retire();
		}
		const timer = setInterval(sync, routePollMilliseconds);
		sync();
		// Cleanup sends no SessionEnd: it also runs on every hot reload,
		// including the one a reinstall triggers.
		return () => {
			clearInterval(timer);
			unsubscribe();
			return pending;
		};
	},
};
