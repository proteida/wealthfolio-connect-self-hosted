import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs, promptOutput } from '../src/index.ts';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

type RunResult = { stdout: string; stderr: string; code: number };

// Runs the CLI as a subprocess with a single empty answer line on stdin.
// stdin is ended explicitly: execFile's input option does not reliably
// close the pipe, which would hang the username prompt.
function runCli(args: string[]): Promise<RunResult> {
	return new Promise((resolve) => {
		const ch = spawn(process.execPath, ['--import', 'tsx', 'src/index.ts', ...args], { cwd: root });
		let stdout = '';
		let stderr = '';
		ch.stdout.on('data', (d) => (stdout += d));
		ch.stderr.on('data', (d) => (stderr += d));
		const timer = setTimeout(() => ch.kill('SIGKILL'), 20000);
		ch.on('close', (code) => {
			clearTimeout(timer);
			resolve({ stdout, stderr, code: code ?? 1 });
		});
		ch.stdin.write('\n');
		ch.stdin.end();
	});
}

describe('parseArgs', () => {
	it('defaults to interactive mode', () => {
		assert.deepEqual(parseArgs([]), { json: false });
	});

	it('parses every flag', () => {
		assert.deepEqual(parseArgs(['--json', '--output', 'f', '--code', 'c', '--username', 'u']), {
			json: true,
			output: 'f',
			code: 'c',
			username: 'u',
		});
	});

	it('accepts a positional username', () => {
		assert.equal(parseArgs(['alice']).username, 'alice');
	});

	it('ignores unknown flags', () => {
		assert.deepEqual(parseArgs(['--nope']), { json: false });
	});
});

describe('promptOutput', () => {
	it('routes prompts to stderr in --json mode', () => {
		assert.equal(promptOutput(true), process.stderr);
	});

	it('routes prompts to stdout otherwise', () => {
		assert.equal(promptOutput(false), process.stdout);
	});
});

describe('steam-auth CLI', () => {
	it('--json keeps stdout machine-readable on EOF', async () => {
		const r = await runCli(['--json']);
		assert.equal(r.code, 2);
		// stdout must carry only the machine-readable payload: with no
		// username there is no payload, and crucially no leaked prompt.
		assert.equal(r.stdout, '');
		assert.match(r.stderr, /username is required/);
	});

	it('interactive prompts go to stdout on EOF', async () => {
		const r = await runCli([]);
		assert.equal(r.code, 2);
		assert.match(r.stdout, /Steam username:/);
	});
});
