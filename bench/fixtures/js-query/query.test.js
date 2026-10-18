import { test } from "node:test";
import assert from "node:assert/strict";

import { parseQuery } from "./query.js";

test("parses key/value pairs", () => {
  assert.deepEqual(parseQuery("?a=1&b=2"), { a: "1", b: "2" });
});

test("decodes percent escapes", () => {
  assert.deepEqual(parseQuery("?q=a%20b"), { q: "a b" });
});

test("flag without value becomes empty string", () => {
  assert.deepEqual(parseQuery("?a=1&flag"), { a: "1", flag: "" });
});

test("empty search yields no keys", () => {
  assert.deepEqual(parseQuery(""), {});
});
