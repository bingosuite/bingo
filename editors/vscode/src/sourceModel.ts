export interface SourceSnippet {
  readonly status: "idle" | "loading" | "ready" | "unavailable";
  readonly message: string;
  readonly lines: readonly {
    readonly number: number;
    readonly text: string;
    readonly highlighted: boolean;
  }[];
}

export const emptySource: SourceSnippet = {
  status: "idle",
  message: "Select a goroutine to inspect its creation site.",
  lines: [],
};
