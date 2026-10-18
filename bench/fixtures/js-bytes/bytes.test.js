import { test } from "node:test";
import assert from "node:assert/strict";

import { formatBytes } from "./bytes.js";

test("bytes below one KiB", () => {
  assert.equal(formatBytes(512), "512 B");
});

test("exactly one KiB keeps one decimal", () => {
  assert.equal(formatBytes(1024), "1.0 KB");
});

test("fractional KiB keeps one decimal", () => {
  assert.equal(formatBytes(1536), "1.5 KB");
});
