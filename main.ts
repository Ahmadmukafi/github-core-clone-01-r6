// Entrypoint for the Go forge: launches the committed static binary.
//
// The trial pod mounts the repository read-only at /bot, and a file uploaded
// through a web UI loses its executable bit, so the binary cannot always be
// executed in place. This launcher tries, in order:
//   1. spawn the binary directly (works when the exec bit survived),
//   2. copy it to a writable temp dir, chmod 0755, and spawn the copy.
// Environment (MAKEFUN_ENV_URL, MAKEFUN_SHOP_TOKEN, PORT) passes through
// untouched; signals and the exit code are forwarded to the child.
import { spawn, type ChildProcess } from 'node:child_process';
import { copyFileSync, chmodSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const binaryPath = join(here, 'forge');

// Spawn a child and wait a short window: a binary that cannot be executed
// (EACCES) fails within milliseconds, while a healthy server stays alive.
function trySpawn(path: string): Promise<ChildProcess> {
  return new Promise((resolve, reject) => {
    const child = spawn(path, [], { env: process.env, stdio: 'inherit' });
    const timer = setTimeout(() => {
      child.off('error', onError);
      resolve(child);
    }, 200);
    const onError = (err: Error): void => {
      clearTimeout(timer);
      reject(err);
    };
    child.once('error', onError);
  });
}

async function launch(): Promise<ChildProcess> {
  if (!existsSync(binaryPath)) {
    throw new Error(`forge binary missing next to main.ts: ${binaryPath}`);
  }
  try {
    return await trySpawn(binaryPath);
  } catch {
    const tmpPath = join(tmpdir(), `forge-${process.pid}`);
    copyFileSync(binaryPath, tmpPath);
    chmodSync(tmpPath, 0o755);
    return await trySpawn(tmpPath);
  }
}

const child = await launch();
child.once('exit', (code) => process.exit(code ?? 0));
child.once('error', (err) => {
  console.error(err);
  process.exit(1);
});
process.on('SIGTERM', () => child.kill('SIGTERM'));
process.on('SIGINT', () => child.kill('SIGINT'));
