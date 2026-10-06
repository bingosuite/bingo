import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { join, resolve } from "node:path";
import { createServer } from "node:net";
import { setTimeout as delay } from "node:timers/promises";

const directory = resolve(process.argv[2]);
const archive = readdirSync(directory).filter((name) => name.endsWith(".tar.gz"));
assert.equal(archive.length, 1);
const scratch = mkdtempSync(join(directory, ".smoke-"));
let child;
try {
  execFileSync("tar", ["-xzf", join(directory, archive[0]), "-C", scratch]);
  const root = join(scratch, archive[0].slice(0, -7));
  const metadata = JSON.parse(readFileSync(join(root, "RELEASE.json"), "utf8"));
  const binary = join(root, "bin", "bingo");
  assert.ok(execFileSync(binary, ["-version"], { encoding: "utf8" }).includes(metadata.commit));
  const port = await freePort();
  const dapPort = await freePort();
  child = spawn(binary, ["-addr", `127.0.0.1:${port}`, "-dap-addr", `127.0.0.1:${dapPort}`, "-idle-timeout", "3s"], { stdio: "inherit" });
  const exited = new Promise((resolveExit, reject) => {
    child.once("error", reject);
    child.once("exit", (code, signal) => resolveExit({ code, signal }));
  });
  let health;
  for (let attempt = 0; attempt < 100; attempt++) {
    try {
      const response = await fetch(`http://127.0.0.1:${port}/api/health`, { signal: AbortSignal.timeout(250) });
      assert.equal(response.status, 200);
      health = await response.json();
      break;
    } catch { await delay(25); }
  }
  assert.equal(health?.service, "bingo");
  assert.equal(health?.wireProtocolVersion, metadata.wireProtocolVersion);
  assert.equal(health?.dap.enabled, true);
  const outcome = await Promise.race([exited, delay(10_000).then(() => { throw new Error("idle exit timed out"); })]);
  assert.deepEqual(outcome, { code: 0, signal: null });
} finally {
  if (child && child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  rmSync(scratch, { recursive: true, force: true });
}

async function freePort() {
  const server = createServer();
  await new Promise((done, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", done); });
  const port = server.address().port;
  await new Promise((done, reject) => server.close((error) => error ? reject(error) : done()));
  return port;
}
