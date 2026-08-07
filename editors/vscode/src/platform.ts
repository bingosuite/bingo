export type SupportedTarget = "darwin-arm64" | "linux-x64";

export interface RuntimePlatform {
  readonly platform: NodeJS.Platform;
  readonly arch: string;
}

export function supportedTargetFor(
  runtime: RuntimePlatform,
): SupportedTarget | undefined {
  if (runtime.platform === "darwin" && runtime.arch === "arm64") {
    return "darwin-arm64";
  }
  if (runtime.platform === "linux" && runtime.arch === "x64") {
    return "linux-x64";
  }
  return undefined;
}
