import { clearTimeout, setTimeout } from "node:timers";

const systemTimers = {
  set(milliseconds, callback) {
    return setTimeout(callback, milliseconds);
  },
  clear(timer) {
    clearTimeout(timer);
  },
};

export async function withTimeout(
  operation,
  milliseconds,
  message,
  timers = systemTimers,
) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = timers.set(milliseconds, () => {
      reject(new Error(message));
    });
    timer.unref?.();
  });
  try {
    return await Promise.race([operation, timeout]);
  } finally {
    if (timer !== undefined) {
      timers.clear(timer);
    }
  }
}
