import process from "node:process";
import { clearTimeout, setTimeout } from "node:timers";

const defaultGracefulTimeoutMs = 2000;
const defaultExitTimeoutMs = 3000;
const defaultGroupTimeoutMs = 2000;
const defaultPollIntervalMs = 20;

export function observeOwnedProcess(child) {
  const pid = child.pid;
  let finished = false;
  let resolveOutcome;
  const outcome = new Promise((resolve) => {
    resolveOutcome = resolve;
  });
  const finish = (value) => {
    if (finished) {
      return;
    }
    finished = true;
    resolveOutcome(value);
  };

  child.once("error", (error) => {
    finish({ kind: "error", error });
  });
  child.once("exit", (code, signal) => {
    finish({ kind: "exit", code, signal });
  });

  return {
    pid,
    outcome,
    isFinished() {
      return finished;
    },
    ref() {
      child.ref();
    },
  };
}

export async function waitForOwnedProcessExit(
  ownedProcess,
  timeoutMs,
  message,
) {
  ownedProcess.ref();
  const outcome = await outcomeWithin(ownedProcess.outcome, timeoutMs);
  if (outcome === undefined) {
    throw new Error(message);
  }
  return outcome;
}

export async function terminateOwnedProcessGroup(
  ownedProcess,
  options = {},
) {
  if (ownedProcess.isFinished()) {
    throw new Error(
      "test-owned server finished before cleanup; refusing to signal a potentially reused PID or process group",
    );
  }
  const pid = ownedProcess.pid;
  if (pid === undefined) {
    throw new Error("test-owned server has no process ID");
  }

  const signalProcess = options.signalProcess ?? process.kill;
  const gracefulTimeoutMs =
    options.gracefulTimeoutMs ?? defaultGracefulTimeoutMs;
  const exitTimeoutMs = options.exitTimeoutMs ?? defaultExitTimeoutMs;
  const groupTimeoutMs = options.groupTimeoutMs ?? defaultGroupTimeoutMs;
  const pollIntervalMs =
    options.pollIntervalMs ?? defaultPollIntervalMs;

  ownedProcess.ref();
  const gracefulSignalDelivered = sendSignal(
    signalProcess,
    pid,
    "SIGTERM",
  );
  const gracefulOutcome = await outcomeWithin(
    ownedProcess.outcome,
    gracefulTimeoutMs,
  );
  if (
    gracefulOutcome === undefined &&
    !ownedProcess.isFinished() &&
    gracefulSignalDelivered &&
    processExists(signalProcess, pid)
  ) {
    sendSignal(signalProcess, -pid, "SIGKILL");
  }

  const outcome =
    gracefulOutcome ??
    (await waitForOwnedProcessExit(
      ownedProcess,
      exitTimeoutMs,
      "test-owned packaged server did not exit after cleanup",
    ));
  await waitForProcessGroupExit(
    pid,
    groupTimeoutMs,
    pollIntervalMs,
    signalProcess,
  );
  return outcome;
}

function sendSignal(signalProcess, pid, signal) {
  try {
    signalProcess(pid, signal);
    return true;
  } catch (error) {
    if (error?.code === "ESRCH") {
      return false;
    }
    throw error;
  }
}

function processExists(signalProcess, pid) {
  try {
    signalProcess(pid, 0);
    return true;
  } catch (error) {
    if (error?.code === "ESRCH") {
      return false;
    }
    if (error?.code === "EPERM") {
      return true;
    }
    throw error;
  }
}

async function waitForProcessGroupExit(
  pid,
  timeoutMs,
  pollIntervalMs,
  signalProcess,
) {
  const deadline = Date.now() + timeoutMs;
  while (processExists(signalProcess, -pid)) {
    const remaining = deadline - Date.now();
    if (remaining <= 0) {
      throw new Error(
        "test-owned packaged server process group remained alive after cleanup",
      );
    }
    await delay(Math.min(pollIntervalMs, remaining));
  }
}

function outcomeWithin(outcome, timeoutMs) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      resolve(undefined);
    }, timeoutMs);
    outcome.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (error) => {
        clearTimeout(timer);
        reject(error);
      },
    );
  });
}

function delay(milliseconds) {
  return new Promise((resolve) => {
    setTimeout(resolve, milliseconds);
  });
}
