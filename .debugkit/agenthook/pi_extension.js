import { existsSync } from "node:fs";

// Pi extension API: https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/docs/extensions.md
// Pi reports a handler's thrown error as an extension error and still runs the
// event and its other handlers:
// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/extensions/runner.ts#L1089-L1117
// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/extensions/runner.ts#L1458-L1466
const replacedSessionReasons = ["new", "resume", "fork"];

// Pi calls this once per session runtime and tears the runtime down when
// another session replaces it, so this state belongs to one session.
export default function (pi) {
	// Events wait until the session file exists, so every reported ID resumes
	// with pi --session. Pi writes the file once the session holds a user
	// message, and never with --no-session:
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/session-manager.ts#L1160-L1189
	let heldStart = null;
	const heldPrompts = [];

	// Pi names no file with --no-session, so nothing would ever flush.
	const persisting = (ctx) => Boolean(ctx.sessionManager.getSessionFile());
	const saved = (ctx) => {
		const file = ctx.sessionManager.getSessionFile();
		return Boolean(file) && existsSync(file);
	};
	const payloadFor = (name, ctx, fields) => {
		const payload = { hook_event_name: name, session_id: ctx.sessionManager.getSessionId(), cwd: ctx.cwd };
		const transcript = ctx.sessionManager.getSessionFile();
		if (transcript) payload.transcript_path = transcript;
		return { ...payload, ...fields };
	};
	const run = (failures, name, payload) => runHooks(name, payload).catch((error) => failures.push(error.message));

	// Sends held events, oldest first, once the session file exists, and reports
	// whether it does.
	const flush = async (ctx, failures) => {
		if (!saved(ctx)) return false;
		if (heldStart) {
			const start = heldStart;
			heldStart = null;
			await run(failures, "session_start", start);
		}
		for (const prompt of heldPrompts.splice(0)) {
			await run(failures, "before_agent_start", prompt);
		}
		return true;
	};

	// Subagents run Pi in json or print mode with global extensions loaded;
	// only the interactive session is one the user can resume:
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/extensions/types.ts#L323-L335
	const on = (name, handler) =>
		pi.on(name, async (event, ctx) => {
			if (ctx.mode !== "tui") return;
			const failures = [];
			await handler(event, ctx, failures);
			if (failures.length > 0) throw new Error(failures.join("; "));
		});

	// Pi reports startup even when it opens a saved session (pi --session or
	// --continue); a session file that already exists means a resume:
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/agent-session.ts#L509
	on("session_start", async (event, ctx, failures) => {
		if (!persisting(ctx)) return;
		const reason = event.reason === "startup" && saved(ctx) ? "resume" : event.reason;
		heldStart = payloadFor("session_start", ctx, { reason });
		await flush(ctx, failures);
	});

	// before_agent_start runs before Pi appends the turn's user message, so a
	// first prompt waits for the next event:
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/agent-session.ts#L2059-L2111
	on("before_agent_start", async (event, ctx, failures) => {
		if (!persisting(ctx)) return;
		if (typeof event.prompt === "string" && event.prompt !== "") {
			heldPrompts.push(payloadFor("before_agent_start", ctx, { prompt: event.prompt }));
		}
		await flush(ctx, failures);
	});

	// context fires before each model call. Pi has appended the user message by
	// the first one: extensions see message_end before Pi persists it, and the
	// agent loop awaits that before streaming the response:
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/agent-session.ts#L1140-L1162
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/agent/src/agent-loop.ts#L117-L124
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/agent/src/agent-loop.ts#L388-L391
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/sdk.ts#L436-L440
	on("context", async (_event, ctx, failures) => {
		if (heldStart || heldPrompts.length > 0) await flush(ctx, failures);
	});

	on("agent_settled", async (_event, ctx, failures) => {
		if (await flush(ctx, failures)) await run(failures, "agent_settled", payloadFor("agent_settled", ctx));
	});

	// Report a shutdown only when another session replaces this one; quit and
	// reload keep the session resumable.
	on("session_shutdown", async (event, ctx, failures) => {
		if (!replacedSessionReasons.includes(event.reason) || !(await flush(ctx, failures))) return;
		await run(failures, "session_shutdown", payloadFor("session_shutdown", ctx, { reason: event.reason }));
	});
}
