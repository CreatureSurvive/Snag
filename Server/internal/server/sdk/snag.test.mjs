// node --test Server/internal/server/sdk/snag.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const Snag = createRequire(import.meta.url)("./snag.js");

test("redaction matches the Swift and server rules", () => {
  const r = (s, level = "standard") => Snag._redact(s, level);
  assert.equal(r("Authorization: Bearer abcdefghijklmnop1234"), "Authorization: Bearer [redacted:bearer]");
  assert.equal(r("password=hunter2&user=me"), "password=[redacted:secret]&user=me");
  assert.equal(r('{"api_key": "sk_live_123456"}'), '{"api_key": "[redacted:secret]"}');
  assert.equal(r("see https://bob:pa55@example.com/x"), "see https://[redacted:credentials]@example.com/x");
  assert.equal(r("card 4242 4242 4242 4242 ok"), "card [redacted:card] ok");
  assert.equal(r("order 1234567890123 isn't a card"), "order 1234567890123 isn't a card");
  assert.equal(r("mail me@example.com"), "mail me@example.com");
  assert.equal(r("mail me@example.com from 10.0.0.12, call +1 555-123-4567", "strict"),
    "mail [redacted:email] from [redacted:ip], call [redacted:phone]");
  assert.equal(r("password=[redacted:secret]"), "password=[redacted:secret]");
  assert.equal(r("password=x123", "off"), "password=x123");
});

test("URLs keep only their shape", () => {
  assert.equal(Snag._sanitizeURL("https://bob:pw@api.example.com:8443/users/12345/items/550e8400-e29b-41d4-a716-446655440000?token=abc&page=2#frag"),
    "api.example.com:8443/users/:id/items/:id?token&page");
  assert.equal(Snag._sanitizeURL("/a/me%40example.com", "https://x.com/"), "x.com/a/:id");
  assert.equal(Snag._sanitizeURL("not a url :::", "nope"), "?");
});
