import { test } from "node:test";
import assert from "node:assert/strict";

import { chunk } from "./chunk.js";

test("splits into full chunks", () => {
  assert.deepEqual(chunk([1, 2, 3, 4], 2), [[1, 2], [3, 4]]);
});

test("keeps the trailing partial chunk", () => {
  assert.deepEqual(chunk([1, 2, 3, 4, 5], 2), [[1, 2], [3, 4], [5]]);
});

test("single chunk when size exceeds length", () => {
  assert.deepEqual(chunk([1, 2], 5), [[1, 2]]);
});

test("rejects non-positive size", () => {
  assert.throws(() => chunk([1], 0));
});
