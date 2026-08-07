import process from "node:process";

const targets = {
  "darwin-arm64": {
    platform: "darwin",
    arch: "arm64",
    goos: "darwin",
    goarch: "arm64",
  },
  "linux-x64": {
    platform: "linux",
    arch: "x64",
    goos: "linux",
    goarch: "amd64",
  },
};

export function currentTarget() {
  const match = Object.entries(targets).find(
    ([, target]) =>
      target.platform === process.platform && target.arch === process.arch,
  );
  if (match === undefined) {
    throw new Error(
      `unsupported VS Code package host ${process.platform}/${process.arch}; expected linux/x64 or darwin/arm64`,
    );
  }
  return match[0];
}

export function targetDetails(target = currentTarget()) {
  const details = targets[target];
  if (details === undefined) {
    throw new Error(`unsupported VS Code package target ${target}`);
  }
  if (details.platform !== process.platform || details.arch !== process.arch) {
    throw new Error(
      `target ${target} requires native host ${details.platform}/${details.arch}, got ${process.platform}/${process.arch}`,
    );
  }
  return { name: target, ...details };
}
