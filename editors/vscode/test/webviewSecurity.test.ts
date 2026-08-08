import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it } from "node:test";

describe("webview security contract", () => {
  const provider = readFileSync(
    resolve(process.cwd(), "src/concurrencyView.ts"),
    "utf8",
  );
  const renderer = readFileSync(
    resolve(process.cwd(), "src/webviewApp.ts"),
    "utf8",
  );

  it("uses a nonce CSP and limits webview resources to dist", () => {
    assert.match(provider, /default-src 'none'/);
    assert.match(provider, /style-src 'nonce-\$\{nonce\}'/);
    assert.match(provider, /script-src 'nonce-\$\{nonce\}'/);
    assert.match(provider, /localResourceRoots: \[dist\]/);
    assert.doesNotMatch(provider, /unsafe-inline|unsafe-eval|https?:\/\//);
  });

  it("renders tracee data through DOM APIs without HTML injection", () => {
    assert.doesNotMatch(renderer, /\.innerHTML|insertAdjacentHTML|eval\s*\(/);
    assert.match(renderer, /\.textContent =/);
    assert.match(renderer, /createElementNS/);
  });

  it("keeps ready/rendered acknowledgements in the host protocol", () => {
    assert.match(provider, /case "ready"/);
    assert.match(provider, /case "rendered"/);
    assert.match(provider, /#delivery/);
    assert.match(provider, /rejectDelivery/);
  });
});
