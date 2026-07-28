import { test } from "node:test";
import assert from "node:assert/strict";
import { computeAccept, encodeTextFrame } from "../../src/gateway/wsHub.js";

test("handshake accept hash matches the RFC 6455 worked example", () => {
  // The example key/answer straight from the spec — if this passes, any
  // real browser will accept our 101 response.
  assert.equal(computeAccept("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=");
});

test("text frames: FIN+text opcode and all three length encodings", () => {
  const small = encodeTextFrame("hi"); // < 126: length lives in byte 1
  assert.equal(small[0], 0x81);
  assert.equal(small[1], 2);
  assert.equal(small.subarray(2).toString(), "hi");

  const medium = encodeTextFrame("x".repeat(300)); // 126 escape + 16-bit length
  assert.equal(medium[1], 126);
  assert.equal(medium.readUInt16BE(2), 300);
  assert.equal(medium.length, 4 + 300);

  const large = encodeTextFrame("y".repeat(70000)); // 127 escape + 64-bit length
  assert.equal(large[1], 127);
  assert.equal(Number(large.readBigUInt64BE(2)), 70000);
  assert.equal(large.length, 10 + 70000);
});
