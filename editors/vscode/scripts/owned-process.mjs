import process from "node:process";
import { clearTimeout, setTimeout } from "node:timers";

const defaultGracefulTimeoutMs = 2000;
const defaultExitTimeoutMs = 3000;
const defaultGroupTimeoutMs = 2000;
const defaultPollIntervalMs = 20;

export function observeOwnedProcess(child, signalProcess = process.kill) {
  const pid = child.pid;
  let finished = false;
  let groupReleased = pid === undefined;
  let groupProbeError;
  let resolveOutcome;
  const outcome = new Promise((resolve) => {
    resolveOutcome = resolve;
  });
  const finish = (value) => {
    if (finished) {
      return;
    }
    try {
      hasProcessGroup();
    } catch {
      // Cleanup reports the saved probe failure without throwing from an event callback.
    }
    finished = true;
    resolveOutcome(value);
  };
  const hasProcessGroup = () => {
    if (groupReleased) {
      return false;
    }
    if (groupProbeError !== undefined) {
      throw groupProbeError;
    }
    try {
      signalProcess(-pid, 0);
      return true;
    } catch (error) {
      if (error?.code === "ESRCH") {
        groupReleased = true;
        return false;
      }
      if (error?.code === "EPERM") {
        return true;
      }
      groupProbeError = error;
      throw error;
    }
  };
  const signalGroup = (signal) => {
    if (!hasProcessGroup()) {
      return false;
    }
    try {
      signalProcess(-pid, signal);
      return true;
    } catch (error) {
      if (error?.code === "ESRCH") {
        groupReleased = true;
        return false;
      }
      throw error;
    }
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
    hasProcessGroup,
    ref() {
      child.ref();
    },
    signalGroup,
    signalLeader(signal) {
      if (finished || pid === undefined) {
        return false;
      }
      return sendSignal(signalProcess, pid, signal);
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
  const gracefulTimeoutMs =
    options.gracefulTimeoutMs ?? defaultGracefulTimeoutMs;
  const exitTimeoutMs = options.exitTimeoutMs ?? defaultExitTimeoutMs;
  const groupTimeoutMs = options.groupTimeoutMs ?? defaultGroupTimeoutMs;
  const pollIntervalMs =
    options.pollIntervalMs ?? defaultPollIntervalMs;

  ownedProcess.ref();
  if (!ownedProcess.isFinished()) {
    ownedProcess.signalLeader("SIGTERM");
  }
  const groupExited = await waitForProcessGroupExit(
    ownedProcess,
    gracefulTimeoutMs,
    pollIntervalMs,
  );
  if (!groupExited) {
    ownedProcess.signalGroup("SIGKILL");
  }

  const outcome = await waitForOwnedProcessExit(
    ownedProcess,
    exitTimeoutMs,
    "test-owned packaged server did not exit after cleanup",
  );
  const groupGone = await waitForProcessGroupExit(
    ownedProcess,
    groupTimeoutMs,
    pollIntervalMs,
  );
  if (!groupGone) {
    throw new Error(
      "test-owned packaged server process group remained alive after cleanup",
    );
  }
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

async function waitForProcessGroupExit(
  ownedProcess,
  timeoutMs,
  pollIntervalMs,
) {
  const deadline = Date.now() + timeoutMs;
  while (ownedProcess.hasProcessGroup()) {
    const remaining = deadline - Date.now();
    if (remaining <= 0) {
      return false;
    }
    await delay(Math.min(pollIntervalMs, remaining));
  }
  return true;
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
