// Pure helpers for the conversation page, with no DOM and no Vue, so they can be tested
// with node --test.

// blocks parses a small, safe subset of Markdown into plain data: paragraphs, bullet and
// numbered lists, and fenced code. Nothing here produces HTML; the page turns the result
// into text nodes. Images, raw HTML and links are kept as the literal text they are, so
// nothing in a reply can load or open anything by itself.
export function blocks(text) {
  const out = [];
  const lines = String(text ?? "").replace(/\r\n?/g, "\n").split("\n");
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.startsWith("```")) {
      const code = [];
      i++;
      while (i < lines.length && !lines[i].startsWith("```")) code.push(lines[i++]);
      i++;
      out.push({ type: "code", text: code.join("\n") });
      continue;
    }
    const bullet = /^\s*[-*]\s+(.*)$/;
    const numbered = /^\s*\d+[.)]\s+(.*)$/;
    if (bullet.test(line) || numbered.test(line)) {
      const ordered = numbered.test(line) && !bullet.test(line);
      const pattern = ordered ? numbered : bullet;
      const items = [];
      while (i < lines.length && pattern.test(lines[i])) items.push(inline(lines[i++].match(pattern)[1]));
      out.push({ type: ordered ? "ol" : "ul", items });
      continue;
    }
    if (line.trim() === "") { i++; continue; }
    const para = [];
    while (i < lines.length && lines[i].trim() !== "" && !lines[i].startsWith("```") && !bullet.test(lines[i]) && !numbered.test(lines[i])) {
      para.push(lines[i++].replace(/^#{1,6}\s+/, ""));
    }
    out.push({ type: "p", spans: inline(para.join("\n")) });
  }
  return out;
}

// inline splits text into spans: plain, bold, italic and code. Everything else, including
// [links](...) and ![images](...), stays plain text.
export function inline(text) {
  const spans = [];
  const pattern = /(`[^`\n]+`|\*\*[^*\n]+\*\*|__[^_\n]+__|\*[^*\s][^*\n]*\*|_[^_\s][^_\n]*_)/g;
  let last = 0;
  for (const m of String(text).matchAll(pattern)) {
    if (m.index > last) spans.push({ type: "text", text: text.slice(last, m.index) });
    const t = m[0];
    if (t.startsWith("`")) spans.push({ type: "code", text: t.slice(1, -1) });
    else if (t.startsWith("**") || t.startsWith("__")) spans.push({ type: "strong", text: t.slice(2, -2) });
    else spans.push({ type: "em", text: t.slice(1, -1) });
    last = m.index + t.length;
  }
  if (last < text.length) spans.push({ type: "text", text: text.slice(last) });
  return spans;
}

const toolPhrases = {
  list_assignments: "Looking at your projects",
  get_assignment: "Reading a project",
  get_task: "Reading a task",
  list_waiting_on_owner: "Checking what is waiting on you",
  recent_activity: "Reading recent activity",
  get_usage: "Checking spend",
  list_tools: "Checking the available tools",
  propose_assignment: "Drafting a new project for you to approve",
  propose_policy: "Drafting a policy change for you to approve",
  propose_schedule: "Drafting a schedule for you to approve",
  propose_route: "Drafting a route for you to approve",
  propose_task: "Drafting a task for you to approve",
  propose_resume: "Asking you to resume",
  pause_assignment: "Pausing",
  forward_owner_message: "Passing your message on",
};

// toolWords describes a tool call in plain words for the working indicator.
export function toolWords(name) {
  if (toolPhrases[name]) return toolPhrases[name] + "…";
  return "Using " + String(name).replace(/__/g, ": ").replace(/_/g, " ") + "…";
}

export const decisionKinds = ["approval", "proposal", "question"];
export const cardKinds = [...decisionKinds, "notification", "report"];

// open reports whether a card still waits for the owner.
export function open(m) {
  return decisionKinds.includes(m.kind) && (m.status === "pending" || m.status === "open");
}

// merge adds or updates a message in a list ordered by seq, replacing a pending local
// message with the stored one that has the same client id.
export function merge(list, m) {
  const out = list.filter((x) => x.id !== m.id && !(x.local && m.client_msg_id && x.client_msg_id === m.client_msg_id));
  out.push(m);
  out.sort((a, b) => (a.seq || Infinity) - (b.seq || Infinity));
  return out;
}

// countdown says how long until a time, in words.
export function countdown(at, now = Date.now()) {
  const ms = new Date(at).getTime() - now;
  if (!(ms > 0)) return "expired";
  const h = Math.floor(ms / 3600000);
  const m = Math.floor((ms % 3600000) / 60000);
  return h > 0 ? `${h}h ${m}m left` : `${m}m left`;
}

export const commands = ["/help", "/status", "/show", "/waiting", "/approve", "/reject", "/answer", "/tell", "/edit",
  "/pause", "/resume", "/halt", "/unhalt", "/retry", "/usage", "/new"];

// suggest lists the slash commands that start with what was typed.
export function suggest(text) {
  if (!text.startsWith("/") || text.includes(" ")) return [];
  return commands.filter((c) => c.startsWith(text.toLowerCase()) && c !== text);
}
