import { h, reactive, onMounted, onUnmounted, nextTick } from "/vue.js";
import { blocks, toolWords, open, merge, countdown, suggest, cardKinds } from "/text.js";
import { icon } from "/icons.js";

// Chat is the conversation with agentd: the owner's and agent's messages, cards for
// what needs the owner, and a composer. Every piece of text from a model, tool or project
// is rendered as text nodes; nothing is ever inserted as HTML.
export function makeChat({ state, api, say, shell, refreshShell, mark }) {
  const store = {
    get(key) { try { return localStorage.getItem(key) || ""; } catch { return ""; } },
    set(key, value) { try { if (value) localStorage.setItem(key, value); else localStorage.removeItem(key); } catch { /* storage blocked */ } },
  };
  const newID = () => (crypto.randomUUID ? crypto.randomUUID() : String(Date.now()) + Math.random().toString(16).slice(2));

  return {
    props: ["about"],
    setup(props) {
      const view = reactive({
        conversations: [], conv: "", messages: [], lastRead: 0, error: "", loading: true,
        working: false, tools: [], partial: {}, draft: "", models: { list: [], tiers: {} }, connected: false, editing: {}, answers: {}, expanded: {},
        announce: "",
      });
      let source;
      let retry = 1000;
      let timer;
      let composer;

      const draftKey = () => "agentd.draft." + view.conv;
      const lastSeq = () => view.messages.reduce((n, m) => (m.seq && m.seq > n ? m.seq : n), 0);

      const connect = () => {
        if (source) source.close();
        source = new EventSource(`/api/conversations/${encodeURIComponent(view.conv)}/stream?after=${lastSeq()}`);
        source.onopen = () => { view.connected = true; retry = 1000; view.partial = {}; };
        source.addEventListener("delta", (e) => {
          const d = JSON.parse(e.data);
          view.partial = { ...view.partial, [d.turn_task_id]: (view.partial[d.turn_task_id] || "") + d.text };
          scrollDown();
        });
        source.addEventListener("message", (e) => {
          const m = JSON.parse(e.data);
          const fresh = !view.messages.some((x) => x.id === m.id);
          view.messages = merge(view.messages, m);
          if (fresh && m.author === "agent") view.announce = "agentd: " + bodyText(m);
          if (fresh && m.author !== "owner") view.tools = [];
          if (m.turn_task_id && m.author !== "owner" && view.partial[m.turn_task_id] !== undefined) {
            const rest = { ...view.partial };
            delete rest[m.turn_task_id];
            view.partial = rest;
          }
          if (fresh) scrollDown();
          markRead();
        });
        source.addEventListener("turn", (e) => {
          const t = JSON.parse(e.data);
          if (t.restarted) {
            const rest = { ...view.partial };
            delete rest[t.turn_task_id];
            view.partial = rest;
          } else if (typeof t.working === "boolean") {
            view.working = t.working;
            if (!t.working) view.tools = [];
          } else if (t.tool) {
            view.tools = [...view.tools.filter((x) => x.action_id !== t.action_id), t].slice(-5);
          }
        });
        source.onerror = async () => {
          view.connected = false;
          source.close();
          const res = await fetch("/ui/session", { credentials: "same-origin" }).catch(() => null);
          if (res && res.status === 401) { state.signedIn = false; return; }
          clearTimeout(timer);
          timer = setTimeout(async () => { await reload(); connect(); }, retry);
          retry = Math.min(retry * 2, 30000);
        };
      };

      const reload = async () => {
        try {
          const msgs = await api("GET", `/api/conversations/${encodeURIComponent(view.conv)}/messages?after=0&limit=500`);
          const pending = view.messages.filter((m) => m.local);
          view.messages = pending.reduce((list, m) => merge(list, m), msgs);
          view.error = "";
        } catch (e) { view.error = e.message; }
      };

      const open_ = async (id) => {
        view.loading = true;
        if (source) source.close();
        view.conv = id;
        view.messages = [];
        view.tools = [];
        view.working = false;
        const c = view.conversations.find((x) => x.id === id);
        view.lastRead = c ? c.last_read : 0;
        view.draft = store.get(draftKey()) || (props.about ? `About ${props.about}: ` : "");
        await reload();
        view.loading = false;
        connect();
        markRead();
        scrollDown();
        focus();
      };

      const loadConversations = async () => {
        view.conversations = await api("GET", "/api/conversations");
      };

      onMounted(async () => {
        try {
          try { const m = await api("GET", "/api/models"); view.models = { list: m.models || [], tiers: m.tiers || {} }; } catch { /* optional */ }
          await loadConversations();
          await open_(view.conversations[0].id);
        } catch (e) { view.error = e.message; view.loading = false; }
      });
      onUnmounted(() => { clearTimeout(timer); if (source) source.close(); });

      let readTimer;
      const markRead = () => {
        clearTimeout(readTimer);
        readTimer = setTimeout(() => {
          const seq = lastSeq();
          if (seq && document.visibilityState !== "hidden") api("POST", `/api/conversations/${encodeURIComponent(view.conv)}/read`, { seq }).catch(() => {});
        }, 500);
      };

      const scrollDown = () => nextTick(() => {
        const end = document.getElementById("chat-end");
        if (end) end.scrollIntoView({ block: "end", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
      });
      const focus = () => nextTick(() => composer && composer.focus());

      const send = async (text, existing) => {
        text = text.trim();
        if (!text) return;
        if (text === "/new") { view.draft = ""; store.set(draftKey(), ""); return startNew(); }
        const local = existing || { id: "local-" + newID(), local: true, client_msg_id: newID(), author: "owner", kind: "text", body: { text } };
        local.sending = true;
        local.failed = false;
        view.messages = merge(view.messages, local);
        if (!existing) { view.draft = ""; store.set(draftKey(), ""); }
        scrollDown();
        try {
          const res = await api("POST", `/api/conversations/${encodeURIComponent(view.conv)}/messages`,
            { text, client_msg_id: local.client_msg_id, ...(local.reply_to ? { reply_to: local.reply_to } : {}) });
          view.messages = merge(view.messages, { ...res.message, client_msg_id: local.client_msg_id });
        } catch (e) {
          view.messages = merge(view.messages, { ...local, sending: false, failed: true, error: e.message });
        }
        focus();
      };

      const startNew = async () => {
        const c = await api("POST", "/api/conversations");
        await loadConversations();
        await open_(c.id);
      };

      const cancel = async () => {
        try { await api("POST", `/api/conversations/${encodeURIComponent(view.conv)}/turn/cancel`); } catch (e) { say("Stop failed: " + e.message); }
      };

      const decide = async (m, approve) => {
        try {
          await api("POST", `/api/approvals/${encodeURIComponent(m.ref_id)}/${approve ? "approve" : "reject"}`);
          say((approve ? "Approved: " : "Rejected: ") + m.body.title);
          await reload();
          refreshShell();
        } catch (e) {
          say("Could not decide: " + e.message);
          await reload();
        }
      };

      const answer = async (m, text) => {
        if (!text.trim()) return;
        try {
          await api("POST", `/api/questions/${encodeURIComponent(m.ref_id)}/answer`, { answer: text });
          view.answers[m.id] = "";
          say("Answered.");
          await reload();
          refreshShell();
        } catch (e) { say("Could not answer: " + e.message); await reload(); }
      };

      const onKey = (e) => {
        if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
          e.preventDefault();
          send(view.draft);
        }
      };

      // render
      const spans = (list) => list.map((s) => s.type === "text" ? s.text : h(s.type === "strong" ? "strong" : s.type === "em" ? "em" : "code", s.text));
      const rich = (text) => blocks(text).map((b) => {
        if (b.type === "code") return h("pre", b.text);
        if (b.type === "ul" || b.type === "ol") return h(b.type, b.items.map((item) => h("li", spans(item))));
        return h("p", spans(b.spans));
      });
      const plain = (text) => String(text || "").split("\n").map((line, i) => (i ? [h("br"), line] : line));

      const outcome = (m) => h("span", { class: "status " + m.status }, m.status === "pending" || m.status === "open" ? "waiting for you" : m.status);
      const kindNames = { proposal: "Proposal", approval: "Approval", question: "Question", notification: "Notification", report: "Report" };
      const starters = ["What are you working on?", "Get me a refund for my cancelled flight", "Every weekday at 8, summarise new email from my accountant"];

      const card = (m) => {
        const c = m.body || {};
        const live = open(m);
        const label = (verb) => `${verb} ${c.title || c.tool || "this"}`;
        const decidable = m.kind === "proposal" || m.kind === "approval" || m.kind === "question";
        const head = [
          h("span", { class: "tile" }, icon(m.kind)),
          h("strong", { class: "kind" }, kindNames[m.kind] || "Report"),
          c.n ? h("span", { class: "muted num" }, "#" + c.n) : null,
          c.assignment ? h("span", { class: "muted" }, c.assignment) : null,
          h("span", { class: "spacer" }),
          c.severity ? h("span", { class: "status " + c.severity }, c.severity) : null,
          decidable && !live ? outcome(m) : null,
        ];
        const parts = [h("div", { class: "head" }, head), h("h3", plain(c.title))];
        if (m.kind === "proposal" || m.kind === "approval") {
          if (c.text) parts.push(h("div", { class: "lines" }, c.text.split("\n").map((line) =>
            h("p", { class: /^(WIDENS|Warning)/.test(line) ? "widen" : "" }, line))));
          if (c.expires_at && live) parts.push(h("p", { class: "muted" }, countdown(c.expires_at)));
          if (c.spec) parts.push(h("details", [h("summary", "As a job file"), h("pre", c.spec)]));
          parts.push(h("details", [h("summary", "Exact arguments"), h("pre", JSON.stringify(c.arguments, null, 2)),
            h("p", { class: "muted" }, "sha256 " + (c.arguments_sha256 || ""))]));
          if (live) {
            const buttons = [
              h("button", { class: "primary", "aria-label": label(m.kind === "proposal" ? "Approve proposal" : "Approve"), onClick: () => decide(m, true) }, "Approve"),
              h("button", { "aria-label": label("Reject"), onClick: () => decide(m, false) }, "Reject"),
            ];
            if (m.kind === "proposal") buttons.push(h("button", { "aria-label": label("Edit"), onClick: () => { view.editing[m.id] = c.spec || JSON.stringify(c.arguments, null, 2); } }, "Edit"));
            parts.push(h("div", { class: "row actions" }, buttons));
            if (view.editing[m.id] !== undefined) {
              parts.push(h("label", { for: "edit-" + m.id }, "Your version (takes effect when saved)"));
              parts.push(h("textarea", { id: "edit-" + m.id, rows: 12, value: view.editing[m.id], onInput: (e) => { view.editing[m.id] = e.target.value; } }));
              parts.push(h("div", { class: "row" }, [
                h("button", { class: "primary", onClick: () => {
                  let edited = view.editing[m.id].trim();
                  if (!edited.startsWith("---")) {
                    try { edited = JSON.stringify(JSON.parse(edited)); } catch { say("That is neither a job file nor valid JSON."); return; }
                  }
                  delete view.editing[m.id];
                  send(`/edit ${c.n} ${edited}`);
                } }, "Save my version"),
                h("button", { onClick: () => { delete view.editing[m.id]; } }, "Cancel"),
              ]));
            }
          }
        } else if (m.kind === "question") {
          if (c.text) parts.push(h("p", plain(c.text)));
          if (live) {
            if (c.choices && c.choices.length) parts.push(h("div", { class: "row" }, c.choices.map((choice) =>
              h("button", { "aria-label": `Answer "${c.title}" with ${choice}`, onClick: () => answer(m, choice) }, choice))));
            parts.push(h("form", { class: "row", onSubmit: (e) => { e.preventDefault(); answer(m, view.answers[m.id] || ""); } }, [
              h("label", { class: "sr-only", for: "answer-" + m.id }, "Your answer to " + c.title),
              h("input", { id: "answer-" + m.id, value: view.answers[m.id] || "", placeholder: "Your answer", onInput: (e) => { view.answers[m.id] = e.target.value; } }),
              h("button", { class: "primary", type: "submit" }, "Answer"),
            ]));
          }
        } else {
          if (c.text) parts.push(h("p", plain(c.text)));
          const links = [];
          if (c.assignment_id) links.push(h("a", { href: "#/a/" + c.assignment_id }, "Open " + (c.assignment || "assignment")));
          if (c.task_id) links.push(h("a", { href: "#/t/" + c.task_id }, "Open task"));
          if (links.length) parts.push(h("p", { class: "row" }, links));
        }
        if (c.source && c.source !== "agentd") parts.push(h("p", { class: "muted small" }, "Text written by the " + c.source + "."));
        return h("article", { class: "card chat-card " + m.kind + (live ? " live" : ""), key: m.id }, parts);
      };

      const bubble = (m) => {
        if (cardKinds.includes(m.kind)) return card(m);
        const text = bodyText(m);
        if (m.author === "owner") {
          return h("div", { class: "bubble owner", key: m.id }, [
            h("div", plain(text)),
            m.sending ? h("div", { class: "note" }, "Sending…") : null,
            m.failed ? h("div", { class: "note", role: "alert" }, ["Not sent: " + (m.error || "error") + " ",
              h("button", { onClick: () => send(text, m) }, "Retry")]) : null,
          ]);
        }
        if (m.author === "agent") return h("div", { class: "turn", key: m.id }, [mark(), h("div", { class: "bubble agent" }, rich(text))]);
        const failed = m.body && m.body.failed;
        return h("div", { class: "bubble system " + m.kind + (failed ? " failed" : ""), key: m.id },
          m.kind === "command_result" ? h("pre", text) : plain(text));
      };

      return shell(() => {
        if (view.loading) return h("p", { class: "muted" }, "Loading…");
        if (view.error && !view.messages.length) return h("p", { role: "alert" }, view.error);
        const away = view.messages.filter((m) => m.seq > view.lastRead && m.author !== "owner" && cardKinds.includes(m.kind));
        const firstAway = view.lastRead > 0 && away.length ? away[0].id : "";
        const suggestions = suggest(view.draft);
        const conv = view.conversations.find((c) => c.id === view.conv) || {};
        const setModel = async (e) => {
          try {
            const res = await api("PUT", `/api/conversations/${encodeURIComponent(view.conv)}/model`, { model: e.target.value });
            say(res.message);
            await loadConversations();
          } catch (err) { say("Model not changed: " + err.message); }
        };
        return h("div", { class: "chat" }, [
          h("div", { class: "chat-head" }, [
            h("h1", { class: "sr-only" }, "Chat"),
            h("label", { class: "sr-only", for: "conv" }, "Conversation"),
            h("select", { id: "conv", value: view.conv, onChange: (e) => open_(e.target.value) }, view.conversations.map((c) =>
              h("option", { value: c.id }, (c.title || "Conversation") + " · " + new Date(c.last_at).toLocaleDateString() + (c.unread ? ` (${c.unread} new)` : "")))),
            h("button", { class: "quiet", onClick: startNew }, [icon("plus"), "New"]),
            h("span", { class: "spacer" }),
            h("span", { class: "muted small", role: "status" }, view.connected ? "" : "Reconnecting…"),
          ]),
          h("div", { class: "timeline", role: "log", "aria-label": "Conversation", "aria-live": "off" }, [
            view.messages.length ? null : h("div", { class: "hero" }, [
              mark(),
              h("h2", "What should agentd work on?"),
              h("p", { class: "muted lede" }, "Describe it in your own words. agentd drafts the job, you approve it, and it reports back here. Type /help for commands."),
              h("div", { class: "starters" }, starters.map((text) =>
                h("button", { type: "button", onClick: () => { view.draft = text; store.set(draftKey(), text); focus(); } }, [text, icon("arrow")]))),
            ]),
            ...view.messages.flatMap((m) => [
              m.id === firstAway ? h("div", { class: "divider", role: "separator", key: "away" },
                `While you were away: ${away.length} new, ${away.filter(open).length} waiting for you`) : null,
              bubble(m),
            ]),
            ...Object.entries(view.partial).map(([turn, text]) =>
              h("div", { class: "turn", key: "partial-" + turn }, [mark(), h("div", { class: "bubble agent provisional", "aria-busy": "true" }, rich(text))])),
            view.working ? h("div", { class: "working", role: "status" }, [
              mark(),
              h("div", { class: "body" }, [
                h("div", { class: "line" }, [
                  h("span", { class: "dots", "aria-hidden": "true" }, [h("i"), h("i"), h("i")]),
                  h("span", "Working"),
                  h("button", { class: "quiet stop", onKeydown: (e) => { if (e.key === "Escape") cancel(); }, onClick: cancel }, "Stop"),
                ]),
                ...view.tools.map((t) => h("div", { class: "muted small" }, toolWords(t.tool))),
              ]),
            ]) : null,
            h("div", { id: "chat-end" }),
          ]),
          h("div", { class: "sr-only", "aria-live": "polite" }, view.announce),
          h("form", { class: "composer", onSubmit: (e) => { e.preventDefault(); send(view.draft); } }, [
            suggestions.length ? h("div", { class: "row suggest", "aria-label": "Commands" }, suggestions.map((s) =>
              h("button", { type: "button", onClick: () => { view.draft = s + " "; focus(); } }, s))) : null,
            h("div", { class: "box" }, [
              h("label", { class: "sr-only", for: "composer" }, "Message"),
              h("textarea", {
                id: "composer", rows: 1, value: view.draft, placeholder: "Message agentd",
                ref: (el) => { composer = el; },
                onInput: (e) => { view.draft = e.target.value; store.set(draftKey(), view.draft); },
                onKeydown: onKey,
              }),
              h("div", { class: "bar" }, [
                h("label", { class: "sr-only", for: "conv-model" }, "Model for this conversation"),
                h("select", { id: "conv-model", value: conv.model || "auto", onChange: setModel }, [
                  h("option", { value: "auto" }, "Model: automatic"),
                  ...["fast", "default", "strong"].map((tier) => h("option", { value: tier }, `Model: ${tier} tier`)),
                  ...view.models.list.map((m) => h("option", { value: m.id }, `Model: ${m.id} (${m.local ? "local" : "cloud"})`)),
                ]),
                h("span", { class: "hint" }, "Enter to send · Shift+Enter for a new line"),
                h("button", { class: "primary send", type: "submit", disabled: !view.draft.trim(), "aria-label": "Send" }, icon("send")),
              ]),
            ]),
          ]),
        ]);
      });
    },
  };
}

function bodyText(m) {
  return (m.body && typeof m.body.text === "string") ? m.body.text : "";
}
