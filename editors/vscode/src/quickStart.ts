import { readdir } from "node:fs/promises";
import { dirname, isAbsolute, relative, sep } from "node:path";

import { ConfigurationError } from "./configuration.js";

interface Resource {
  readonly scheme: string;
  readonly fsPath: string;
}

export interface GoPackageContext {
  readonly activeDocument?: Resource & {
    readonly languageId: string;
    readonly isUntitled: boolean;
  };
  readonly folder?: Resource;
  readonly workspaceFolders: readonly Resource[];
}

export interface GoPackageDebugConfiguration {
  readonly type: "bingo";
  readonly request: "launch";
  readonly name: string;
  readonly mode: "debug";
  readonly program: string;
  readonly cwd: string;
}

interface DirectoryEntry {
  readonly name: string;
  isFile(): boolean;
}

type ReadPackageDirectory = (path: string) => Promise<readonly DirectoryEntry[]>;

export function needsGoPackageConfiguration(
  config: Readonly<Record<string, unknown>>,
): boolean {
  return Object.entries(config).every(([key, value]) =>
    (key === "type" && (value === "bingo" || value === "" || value === undefined)) ||
    ((key === "name" || key === "request") && (value === "" || value === undefined)),
  );
}

export async function goPackageConfiguration(
  context: GoPackageContext,
  readDirectory: ReadPackageDirectory = (path) => readdir(path, { withFileTypes: true }),
): Promise<GoPackageDebugConfiguration> {
  const program = await packageDirectory(context, readDirectory);
  return {
    type: "bingo",
    request: "launch",
    name: "bingo: Debug Go Package",
    mode: "debug",
    program,
    cwd: program,
  };
}

async function packageDirectory(
  context: GoPackageContext,
  readDirectory: ReadPackageDirectory,
): Promise<string> {
  const active = context.activeDocument;
  if (active?.languageId === "go") {
    if (active.isUntitled) {
      throw new ConfigurationError("Save the Go file before debugging its package.");
    }
    const path = localPath(active);
    if (context.folder === undefined || isInside(localPath(context.folder), path)) {
      return dirname(path);
    }
  }

  const folder = context.folder ??
    (context.workspaceFolders.length === 1 ? context.workspaceFolders[0] : undefined);
  if (folder === undefined) {
    throw new ConfigurationError(
      "Open a saved Go file to debug its package, or choose a workspace folder containing Go source. Bingo will not guess a package in a multi-root or empty workspace.",
    );
  }
  const path = localPath(folder);
  let entries: readonly DirectoryEntry[];
  try {
    entries = await readDirectory(path);
  } catch (error: unknown) {
    throw new ConfigurationError(
      `Cannot read Go package directory ${path}: ${error instanceof Error ? error.message : String(error)}`,
      { cause: error },
    );
  }
  if (!entries.some((entry) =>
    entry.isFile() && entry.name.endsWith(".go") &&
    !entry.name.endsWith("_test.go") && !/^[._]/u.test(entry.name),
  )) {
    throw new ConfigurationError(
      `No Go source files in ${path}. Open a Go file in the package you want to debug, or set mode "debug" and program to its directory in launch.json.`,
    );
  }
  return path;
}

function localPath(resource: Resource): string {
  if (resource.scheme !== "file" || !isAbsolute(resource.fsPath)) {
    throw new ConfigurationError(
      'Automatic package selection requires a saved local Go file or local workspace folder. For a remote server, use an explicit server-local program with serverMode "connectOnly".',
    );
  }
  return resource.fsPath;
}

function isInside(folder: string, path: string): boolean {
  const suffix = relative(folder, path);
  return suffix !== ".." && !suffix.startsWith(`..${sep}`) && !isAbsolute(suffix);
}
