// One-time Steam authentication for the Go service.
//
// Flow: username/password (+ Steam Guard) -> WebBrowser refresh token.
// The refresh token is a password-equivalent secret: it is printed or
// written on explicit request only, never logged with anything else, and
// the password/guard codes are never persisted anywhere.
//
// Usage:
//   npm run steam-auth                  # interactive, human-readable
//   npm run steam-auth -- --json         # machine-readable JSON on stdout
//   npm run steam-auth -- --output FILE  # also write credentials to FILE (0600)

import * as fs from 'node:fs';
import * as readline from 'node:readline';
import * as path from 'node:path';
import { pathToFileURL } from 'node:url';
import { EAuthTokenPlatformType, LoginSession } from 'steam-session';

export type Args = { json: boolean; output?: string; code?: string; username?: string };

export function parseArgs(argv: string[]): Args {
	const out: Args = { json: false };
	for (let i = 0; i < argv.length; i++) {
		const a = argv[i];
		if (a === '--json') out.json = true;
		else if (a === '--output') out.output = argv[++i];
		else if (a === '--code') out.code = argv[++i];
		else if (a === '--username') out.username = argv[++i];
		else if (!a.startsWith('--') && out.username === undefined) out.username = a;
	}
	return out;
}

function ask(rl: readline.Interface, q: string): Promise<string> {
	return new Promise((resolve) => {
		let done = false;
		const finish = (v: string) => {
			if (!done) {
				done = true;
				resolve(v);
			}
		};
		// EOF (closed pipe, /dev/null) resolves empty instead of hanging
		// or letting the process exit 0 with no verdict.
		rl.question(q, (ans) => finish(ans ?? ''));
		rl.once('close', () => finish(''));
	});
}

// promptOutput routes interactive prompts to stderr in --json mode so
// stdout carries only the machine-readable payload.
export function promptOutput(json: boolean): NodeJS.WriteStream {
	return (json ? process.stderr : process.stdout) as NodeJS.WriteStream;
}

// askSecret reads a line without echoing it. The value lives in memory
// only for the login call and is never written anywhere. Prompts go to
// out (stderr in --json mode) so stdout stays machine-readable.
function askSecret(prompt: string, out: NodeJS.WriteStream = process.stdout): Promise<string> {
	return new Promise((resolve) => {
		const rl = readline.createInterface({ input: process.stdin, output: out });
		const stdin = process.stdin;
		const finish = (v: string) => {
			stdin.removeListener('data', onData);
			stdin.removeListener('end', onEnd);
			stdin.setRawMode?.(false);
			stdin.pause();
			out.write('\n');
			rl.close();
			resolve(v);
		};
		const onEnd = () => finish(buf.trim());
		const onData = (ch: Buffer) => {
			const s = ch.toString('utf8');
			if (s === '\n' || s === '\r' || s === '\u0004') {
				finish(buf.trim());
			} else if (s === '\u0003') {
				stdin.removeListener('data', onData);
				stdin.removeListener('end', onEnd);
				stdin.setRawMode?.(false);
				stdin.pause();
				rl.close();
				process.exit(130);
			} else if (s === '\u007f') {
				buf = buf.slice(0, -1);
			} else {
				buf += s;
			}
		};
		let buf = '';
		out.write(prompt);
		stdin.setRawMode?.(true);
		stdin.resume();
		stdin.on('data', onData);
		// EOF without Enter fails closed instead of hanging on a secret
		// prompt forever.
		stdin.once('end', onEnd);
	});
}

async function main(): Promise<void> {
	const args = parseArgs(process.argv.slice(2));
	const say = (msg: string) => {
		if (!args.json) console.log(msg);
	};

	const out = promptOutput(args.json);
	const rl = readline.createInterface({ input: process.stdin, output: out });
	const username = args.username ?? (await ask(rl, 'Steam username: ')).trim();
	rl.close();
	if (!username) {
		console.error('username is required');
		process.exit(2);
	}
	// Read from TTY only; the password is used once and never stored.
	const password = await askSecret('Steam password (hidden): ', out);
	if (!password) {
		console.error('password is required');
		process.exit(2);
	}

	const session = new LoginSession(EAuthTokenPlatformType.WebBrowser);
	// Guard against hanging forever on mobile approval.
	session.loginTimeout = 120000;

	const done = new Promise<void>((resolve, reject) => {
		session.on('authenticated', () => resolve());
		session.on('timeout', () => reject(new Error('login timed out waiting for approval')));
		session.on('error', (err: Error) => reject(err));
	});

	// A code supplied up front (email or TOTP) is attempted first.
	const started = session.startWithCredentials({
		accountName: username,
		password,
		...(args.code ? { steamGuardCode: args.code } : {}),
	});
	// password drops out of scope after this; it is never stored or printed.

	const pollForGuard = async (): Promise<void> => {
		try {
			await started;
		} catch (err) {
			// Guard rejections surface here; interactive fallback below.
			if (!/auth|guard|code|confirm|poll/i.test((err as Error)?.message ?? '')) throw err;
		}
	};

	// If polling requests a code we did not supply, ask for it.
	session.on('polling', () => {
		void (async () => {
			try {
				const res: any = (session as any)._startSessionResponse;
				const guards: string[] = (res?.allowedConfirmations ?? []).map((c: any) => c?.confirmationType ?? c?.type ?? '');
				const needsCode = guards.some((g) => /code/i.test(String(g)));
				const needsApproval = guards.some((g) => /confirmation/i.test(String(g)));
				if (needsCode && !args.code) {
					const rl2 = readline.createInterface({ input: process.stdin, output: promptOutput(args.json) });
					const code = (await ask(rl2, 'Steam Guard code (email or app): ')).trim();
					rl2.close();
					if (code) await session.submitSteamGuardCode(code);
				} else if (needsApproval) {
					say('Approve the login on your mobile device (waiting up to 120s)...');
				}
			} catch (err) {
				say(`guard handling failed: ${(err as Error).message}`);
			}
		})();
	});

	await pollForGuard();
	await done;

	const steamID = session.steamID?.getSteamID64?.() ?? '';
	const refreshToken = (session as any).refreshToken as string | undefined;
	if (!steamID || !refreshToken) {
		throw new Error('authenticated but no steamID/refresh token was issued');
	}

	const payload = { steam_id: steamID, refresh_token: refreshToken, platform: 'web' };
	if (args.output) {
		fs.writeFileSync(args.output, JSON.stringify(payload, null, 2) + '\n', { mode: 0o600 });
		try {
			fs.chmodSync(args.output, 0o600);
		} catch {
			// best effort on non-POSIX filesystems
		}
		if (!args.json) {
			console.log(`WARNING: ${args.output} contains a password-equivalent secret. Keep it out of git and backups.`);
			console.log(`wrote credentials to ${args.output} (mode 0600)`);
		}
	}
	if (args.json) {
		console.log(JSON.stringify(payload));
	} else {
		console.log('authenticated as steamID ' + steamID);
		console.log('Set in the service environment:');
		console.log(`  STEAM_ID=${steamID}`);
		console.log('  STEAM_REFRESH_TOKEN=<the refresh token above>');
		if (!args.output) {
			console.log('Refresh token (copy now, it will not be shown again):');
			console.log('  ' + refreshToken);
		}
	}
}

// Tests import this module for parseArgs/promptOutput without running the
// login flow: main runs only when the file is the CLI entrypoint.
const invokedAsMain = (() => {
	try {
		const entry = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : '';
		return import.meta.url === entry;
	} catch {
		return false;
	}
})();
if (invokedAsMain) {
	main().catch((err: unknown) => {
		console.error('steam-auth failed: ' + (err instanceof Error ? err.message : String(err)));
		process.exit(1);
	});
}
