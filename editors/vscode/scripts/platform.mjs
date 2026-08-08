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
  const requested = process.env.BINGO_VSCODE_TARGET;
  if (requested !== undefined) {
    validateBuilder(requested);
    return requested;
  }
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
  validateBuilder(target);
  return { name: target, ...details };
}

function validateBuilder(target) {
  const details = targets[target];
  if (details === undefined) {
    throw new Error(`unsupported VS Code package target ${target}`);
  }
  const native =
    details.platform === process.platform && details.arch === process.arch;
  const darwinCrossBuild =
    target === "darwin-arm64" &&
    process.platform === "darwin" &&
    process.arch === "x64";
  const linuxCrossBuild =
    target === "linux-x64" &&
    process.platform === "darwin" &&
    (process.arch === "arm64" || process.arch === "x64");
  if (!native && !darwinCrossBuild && !linuxCrossBuild) {
    throw new Error(
      `target ${target} cannot be packaged from ${process.platform}/${process.arch}`,
    );
  }
}
