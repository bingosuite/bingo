import process from "node:process";

import { withTimeout } from "./timing.mjs";

export async function terminateOwnedProcessGroup(
  pid,
  exit,
  signalProcess = process.kill,
) {
  try {
    signalProcess(-pid, "SIGKILL");
  } catch (error) {
    if (error?.code !== "ESRCH") {
      throw error;
    }
  }
  await withTimeout(
    exit.catch(() => undefined),
    2000,
    "test-owned packaged server did not exit after emergency cleanup",
  );
}
