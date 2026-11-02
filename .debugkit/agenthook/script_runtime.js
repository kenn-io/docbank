import { spawn } from "node:child_process";

const defaultTimeoutSeconds = 60;
// A failure quotes at most this many characters from the end of the command's stderr.
const stderrTailLength = 2000;
// A background process the command starts can inherit stderr and hold it open,
// so the wait ends this long after the command itself exits.
const stderrGraceMs = 250;

const exitFailure = (code, signal) => {
	if (signal) return `killed by ${signal}`;
	return code === 0 ? null : `exited with status ${code}`;
};

// Runs one argv without a shell in the payload's directory, writing the payload
// to stdin, and resolves to a failure description, ending in the tail of the
// command's stderr, or null. At its timeout the child is force-killed and the
// wait ends even if it has not exited, so a child that ignores SIGTERM cannot
// block later commands or the harness.
function runCommand(handler, payload) {
	return new Promise((resolve) => {
		let child;
		try {
			// Pi changes ctx.cwd on a cross-project resume without changing the process directory.
			child = spawn(handler.command, handler.args, { cwd: payload.cwd, stdio: ["pipe", "ignore", "pipe"] });
		} catch (error) {
			resolve(`could not start: ${error.message}`);
			return;
		}
		let stderr = "";
		let done = false;
		let grace;
		const seconds = handler.timeout > 0 ? handler.timeout : defaultTimeoutSeconds;
		const timer = setTimeout(() => {
			child.kill("SIGKILL");
			finish(`timed out after ${seconds}s`);
		}, seconds * 1000);
		const finish = (failure) => {
			if (done) return;
			done = true;
			clearTimeout(timer);
			clearTimeout(grace);
			// Keep draining stderr so a process still writing to it never blocks,
			// without holding the harness open.
			child.stderr.unref();
			const detail = stderr.trim();
			resolve(failure && detail ? `${failure}: ${detail}` : failure);
		};
		child.stderr.setEncoding("utf8");
		child.stderr.on("data", (chunk) => {
			if (!done) stderr = (stderr + chunk).slice(-stderrTailLength);
		});
		child.once("error", (error) => finish(`could not start: ${error.message}`));
		child.once("exit", (code, signal) => {
			// The command finished in time, so only the stderr wait remains.
			clearTimeout(timer);
			grace = setTimeout(() => finish(exitFailure(code, signal)), stderrGraceMs);
		});
		child.once("close", (code, signal) => finish(exitFailure(code, signal)));
		child.stdin.on("error", () => {});
		child.stdin.end(JSON.stringify(payload));
	});
}

// Runs every registered command for a native event in order, then throws one
// error naming each command that failed so the harness reports it.
async function runHooks(event, payload) {
	const failures = [];
	for (const entry of config.hooks?.[event] ?? []) {
		for (const handler of entry.hooks) {
			const failure = await runCommand(handler, payload);
			if (failure) failures.push(`${[handler.command, ...handler.args].join(" ")}: ${failure}`);
		}
	}
	if (failures.length > 0) {
		throw new Error(`agenthook ${event} commands failed: ${failures.join("; ")}`);
	}
}
