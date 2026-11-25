import { createApp, h, reactive, onMounted, onUnmounted, watch } from "/vue.js";
import { makeChat } from "/chat.js";
import { icon } from "/icons.js";

const state = reactive({
  csrf: null,
  signedIn: null,
  route: location.hash.slice(1) || "/",
  system: { halted: false },
  counts: { approvals: 0, unresolved: 0, questions: 0 },
  servers: [],
  toast: "",
  dialog: null,
});

// ask opens an in-page dialog and resolves to the typed text ("" when there is no
// input) or null when the owner cancels.
function ask(options) {
  return new Promise((resolve) => { state.dialog = { value: "", ...options, resolve }; });
}
const confirmBox = async (title, body, options) => (await ask({ title, body, ...options })) !== null;
const promptBox = (title, body, options) => ask({ title, body, input: true, ...options });
function closeDialog(result) {
  const d = state.dialog;
  state.dialog = null;
  if (d) d.resolve(result);
}
function dialogView() {
  const d = state.dialog;
  if (!d) return null;
  return h("dialog", {
    class: "modal", "aria-labelledby": "dialog-title",
    ref: (el) => { if (el && !el.open) el.showModal(); },
    onCancel: (e) => { e.preventDefault(); closeDialog(null); },
  }, h("form", { onSubmit: (e) => { e.preventDefault(); closeDialog(d.input ? d.value : ""); } }, [
    h("h2", { id: "dialog-title" }, d.title),
    d.body ? h("p", { class: "muted pre-line" }, d.body) : null,
    d.input ? [
      h("label", { class: "sr-only", for: "dialog-input" }, d.label || d.title),
      h("input", { id: "dialog-input", value: d.value, placeholder: d.placeholder || "", autofocus: true, onInput: (e) => { d.value = e.target.value; } }),
    ] : null,
    h("div", { class: "row end" }, [
      h("button", { type: "button", onClick: () => closeDialog(null) }, "Cancel"),
      h("button", { type: "submit", class: d.danger ? "danger" : "primary" }, d.confirm || "Confirm"),
    ]),
  ]));
}

window.addEventListener("hashchange", () => { state.route = location.hash.slice(1) || "/"; });

let toastTimer;
function say(text) {
  state.toast = text;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { state.toast = ""; }, 5000);
}

async function api(method, path, body) {
  const headers = { Accept: "application/json" };
  if (method !== "GET") headers["X-CSRF-Token"] = state.csrf || "";
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: "same-origin" });
  if (res.status === 401) {
    state.signedIn = false;
    throw new Error("signed out");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

async function act(label, method, path, body) {
  try {
    await api(method, path, body);
    say(label + ": done");
    await refreshShell();
    return true;
  } catch (e) {
    say(label + " failed: " + e.message);
    return false;
  }
}

async function refreshShell() {
  try {
    const [system, approvals, unresolved, servers, questions] = await Promise.all([
      api("GET", "/api/system"), api("GET", "/api/approvals"), api("GET", "/api/actions/unresolved"), api("GET", "/api/mcp/servers"),
      api("GET", "/api/questions"),
    ]);
    state.system = system;
    state.servers = servers;
    state.counts = { approvals: approvals.length, unresolved: unresolved.length, questions: questions.length };
  } catch (e) { /* the view shows its own error */ }
}

const fmtTime = (t) => (t ? new Date(t).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" }) : "");
const money = (n) => "$" + Number(n || 0).toFixed(2);
const pretty = (v) => { try { return JSON.stringify(typeof v === "string" ? JSON.parse(v) : v, null, 2); } catch { return String(v); } };
const status = (s) => h("span", { class: "status " + s }, s.replace(/_/g, " "));
const link = (to, text) => h("a", { href: "#" + to }, text);
const cap = (s) => s[0].toUpperCase() + s.slice(1);
const pageHead = (title, sub, extra) => h("div", { class: "page-head" }, [h("h1", title), extra || null, sub ? h("p", { class: "sub" }, sub) : null]);
const sectionHead = (title, count) => h("div", { class: "section-head" }, [h("h2", title), count ? h("span", { class: "count num" }, String(count)) : null]);
const loading = () => h("p", { class: "muted" }, "Loading…");
const failed = (message) => h("p", { class: "status failed", role: "alert" }, message);
const mark = () => h("span", { class: "mark", "aria-hidden": "true" }, "B");

function loader(fetcher) {
  const view = reactive({ data: null, error: "", loading: true });
  const load = async () => {
    try {
      view.data = await fetcher();
      view.error = "";
    } catch (e) {
      view.error = e.message;
    } finally {
      view.loading = false;
    }
  };
  return { view, load };
}

const stopAll = async () => {
  const reason = await promptBox("Emergency stop", "No task starts, no tool runs and no model call is made until you lift it.",
    { placeholder: "Reason (optional)", label: "Reason", confirm: "Stop everything", danger: true });
  if (reason !== null) act("Emergency stop", "POST", "/api/system/emergency-stop", { reason });
};
const liftStop = async () => {
  if (await confirmBox("Lift the emergency stop?", "agentd will start working again.", { confirm: "Lift stop" })) act("Resume", "POST", "/api/system/resume");
};
const signOut = async () => { await api("DELETE", "/ui/session").catch(() => {}); state.signedIn = false; state.csrf = null; };

function shell(content) {
  return () => {
    if (state.signedIn === false) return h(Login);
    if (state.signedIn === null) return h("main", loading());
    const items = [["/", "Chat", "chat"], ["/work", "Work", "work"], ["/inbox", "Inbox", "inbox"], ["/tools", "Tools", "tools"], ["/activity", "Activity", "activity"]];
    const waiting = state.counts.approvals + state.counts.unresolved + state.counts.questions;
    const troubled = state.servers.filter((s) => s.state === "needs_auth" || s.state === "failed");
    const reviews = state.servers.filter((s) => s.state === "needs_review").length;
    const halted = state.system.halted;
    const current = (to) => state.route === to || (to === "/" && state.route.startsWith("/?")) || (to === "/work" && /^\/[at]\//.test(state.route));
    return h("div", { class: "app" }, [
      h("aside", { class: "side" }, [
        h("a", { class: "brand", href: "#/" }, [mark(), "agentd"]),
        h("nav", { "aria-label": "Main" }, items.map(([to, text, ic]) =>
          h("a", { href: "#" + to, class: current(to) ? "active" : "", "aria-current": current(to) ? "page" : null }, [
            icon(ic), h("span", { class: "label" }, text),
            to === "/inbox" && waiting ? h("span", { class: "badge", "aria-label": waiting + " waiting" }, String(waiting)) : null,
            to === "/tools" && troubled.length + reviews ? h("span", { class: "badge", "aria-label": (troubled.length + reviews) + " need attention" }, String(troubled.length + reviews)) : null,
          ]))),
        h("div", { class: "side-foot" }, [
          h("div", { class: "sys " + (halted ? "off" : "on") }, [h("span", { class: "dot" }), h("span", { class: "label" }, halted ? "Stopped" : "Running")]),
          halted ? null : h("button", { class: "quiet stop", onClick: stopAll }, [icon("stop"), h("span", { class: "label" }, "Emergency stop")]),
          h("button", { class: "quiet", onClick: signOut }, [icon("signout"), h("span", { class: "label" }, "Sign out")]),
        ]),
      ]),
      h("div", { class: "content" }, [
        halted
          ? h("div", { class: "alert halted", role: "alert" }, [
              h("strong", "Emergency stop is on."),
              h("span", state.system.reason || ""),
              h("span", { class: "spacer" }),
              h("button", { onClick: liftStop }, "Lift stop"),
            ])
          : null,
        troubled.length && state.route !== "/tools"
          ? h("div", { class: "alert trouble", role: "alert" }, [
              h("strong", "Tools unavailable:"),
              h("span", troubled.map((s) => s.server + " " + s.state.replace("_", " ")).join(", ")),
              link("/tools", "Details"),
            ])
          : null,
        h("main", content()),
      ]),
      h("div", { class: "sr-only", role: "status", "aria-live": "polite" }, state.toast),
      state.toast ? h("div", { class: "toast", "aria-hidden": "true" }, state.toast) : null,
      dialogView(),
    ]);
  };
}

const Login = {
  setup() {
    const form = reactive({ token: "", error: "", busy: false });
    const submit = async (e) => {
      e.preventDefault();
      form.busy = true;
      try {
        const res = await fetch("/ui/session", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token: form.token }), credentials: "same-origin" });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || "sign-in failed");
        state.csrf = data.csrf;
        form.token = "";
        state.signedIn = true;
        refreshShell();
      } catch (err) {
        form.error = err.message;
      } finally {
        form.busy = false;
      }
    };
    return () => h("main", { class: "login" }, h("form", { class: "card", onSubmit: submit }, [
      h("div", { class: "brand" }, [mark(), "agentd"]),
      h("h1", "Sign in"),
      h("p", { class: "muted" }, "Paste the API token from the api-token file. It is sent once; the browser keeps only a session cookie it cannot read."),
      h("label", { for: "token" }, "API token"),
      h("input", { id: "token", type: "password", autocomplete: "off", value: form.token, onInput: (e) => { form.token = e.target.value; } }),
      form.error ? h("p", { class: "error", role: "alert" }, form.error) : null,
      h("button", { class: "primary", type: "submit", disabled: form.busy || !form.token }, "Sign in"),
    ]));
  },
};

const Assignments = {
  setup() {
    const { view, load } = loader(() => api("GET", "/api/assignments"));
    onMounted(load);
    return shell(() => {
      if (view.loading) return loading();
      if (view.error) return failed(view.error);
      if (!view.data.length) return [pageHead("Work"), h("div", { class: "empty" }, [
        h("h2", "Nothing assigned yet"),
        h("p", ["Describe what you want in the ", link("/", "chat"), ", or create a job with agentd create."]),
      ])];
      return [
        pageHead("Work", "Every project and responsibility, with what happens next."),
        h("div", { class: "grid" }, view.data.map((a) => h("article", { class: "card" }, [
          h("div", { class: "row" }, [status(a.status), h("span", { class: "eyebrow" }, a.kind)]),
          h("h2", link("/a/" + a.id, a.name)),
          a.status_reason ? h("p", { class: "muted small" }, a.status_reason) : null,
          h("div", { class: "next" }, [h("span", { class: "eyebrow" }, "Next"), a.next.what, a.next.at ? h("span", { class: "muted" }, " · " + fmtTime(a.next.at)) : null]),
          a.pending_approvals || a.blocked_tasks || a.overdue_obligations ? h("div", { class: "row" }, [
            a.pending_approvals ? h("span", { class: "chip warn" }, a.pending_approvals + " to approve") : null,
            a.blocked_tasks ? h("span", { class: "chip bad" }, a.blocked_tasks + " blocked") : null,
            a.overdue_obligations ? h("span", { class: "chip bad" }, a.overdue_obligations + " overdue") : null,
          ]) : null,
          h("div", { class: "meter" }, [
            h("span", { class: "num" }, ["Spent ", money(a.spent_usd), a.budget_usd ? " of " + money(a.budget_usd) : ""]),
            a.budget_usd ? h("progress", { class: a.spent_usd / a.budget_usd > 0.8 ? "high" : "", max: a.budget_usd, value: Math.min(a.spent_usd, a.budget_usd), "aria-label": "Budget used" }) : null,
          ]),
        ]))),
      ];
    });
  },
};

const lifecycle = {
  project: { active: ["pause", "complete", "cancel"], paused: ["resume", "complete", "cancel"] },
  responsibility: { active: ["pause", "revoke"], paused: ["resume", "revoke"] },
};

const Assignment = {
  props: ["id"],
  setup(props) {
    const { view, load } = loader(() => api("GET", "/api/assignments/" + props.id));
    onMounted(load);
    watch(() => props.id, load);
    const run = async (action) => {
      if (action !== "pause" && action !== "resume" && !await confirmBox(`${cap(action)} this assignment?`, "This cannot be undone.", { confirm: cap(action), danger: true })) return;
      if (await act(action, "POST", `/api/assignments/${props.id}/${action}`)) load();
    };
    const confirmCriterion = async (c) => {
      const note = await promptBox("Confirm this yourself?", c.description, { placeholder: "Note (optional)", label: "Note", confirm: "I verified this" });
      if (note !== null && await act("Confirm", "POST", `/api/assignments/${props.id}/criteria/${encodeURIComponent(c.id)}/confirm`, { note })) load();
    };
    return shell(() => {
      if (view.loading) return loading();
      if (view.error) return failed(view.error);
      const a = view.data;
      const actions = (lifecycle[a.kind] || {})[a.status] || [];
      return [
        h("p", { class: "small" }, link("/work", "← Work")),
        pageHead(a.name, null, [status(a.status), h("span", { class: "eyebrow" }, a.kind)]),
        h("section", { class: "card" }, [
          a.status_reason ? h("p", { class: "muted" }, a.status_reason) : null,
          h("p", { class: "lede pre-line" }, a.objective),
          h("div", { class: "next" }, [h("span", { class: "eyebrow" }, "Next"), a.next.what]),
          (a.policy_warnings || []).map((w) => h("p", { class: "status pending", role: "note" }, "Policy: " + w)),
          h("div", { class: "meter" }, [
            h("span", { class: "num" }, ["Spent ", money(a.spent_usd), a.budget_usd ? ` of ${money(a.budget_usd)}${a.budget_period ? " per " + a.budget_period : ""}` : "",
              a.reserved_usd ? ` (+${money(a.reserved_usd)} held)` : ""]),
            a.budget_usd ? h("progress", { class: a.spent_usd / a.budget_usd > 0.8 ? "high" : "", max: a.budget_usd, value: Math.min(a.spent_usd, a.budget_usd), "aria-label": "Budget used" }) : null,
          ]),
          h("div", { class: "row" }, [
            h("a", { class: "btn", href: "#/?about=" + a.id }, [icon("chat"), "Ask about this"]),
            h("span", { class: "spacer" }),
            ...actions.map((x) => h("button", { class: x === "pause" || x === "resume" ? "" : "quiet stop", onClick: () => run(x) }, cap(x))),
          ]),
        ]),
        a.criteria && a.criteria.length ? h("section", { class: "card" }, [
          h("h3", "Completion criteria"),
          h("table", h("tbody", a.criteria.map((c) => h("tr", [
            h("td", c.description),
            h("td", c.verified ? [status("verified"), h("div", { class: "muted" }, `by ${c.evidence}${c.event_id ? " " + c.event_id : ""} · ${fmtTime(c.verified_at)}`)] : status("pending")),
            h("td", c.verified || a.status === "completed" ? null : h("button", { onClick: () => confirmCriterion(c) }, "I verified this")),
          ])))),
        ]) : null,
        a.obligations && a.obligations.length ? h("section", { class: "card" }, [
          h("h3", "Obligations"),
          h("table", h("tbody", a.obligations.map((o) => h("tr", [
            h("td", [o.description, h("div", { class: "muted" }, `every ${o.every} · ${o.evidence}`)]),
            h("td", o.overdue ? h("span", { class: "status failed" }, `overdue ×${o.missed_intervals}`) : [status("ok"), h("div", { class: "muted" }, "due " + fmtTime(o.due_at))]),
            h("td", { class: "muted" }, o.last_evidence_at ? "last " + fmtTime(o.last_evidence_at) : "no evidence yet"),
          ])))),
        ]) : null,
        h("section", { class: "card" }, [
          h("h3", "Open tasks"),
          a.open_tasks.length ? h("table", h("tbody", a.open_tasks.map((t) => h("tr", [h("td", link("/t/" + t.id, t.title)), h("td", [status(t.status), t.waiting ? h("div", { class: "muted" }, t.waiting) : null]), h("td", { class: "muted" }, fmtTime(t.next_run_at))])))) : h("p", { class: "muted" }, "None."),
        ]),
        h("section", { class: "card" }, [
          h("h3", "Recent activity"),
          h("div", { class: "scroll-x" }, h("table", h("tbody", a.recent.map((r) => h("tr", [h("td", { class: "muted num" }, fmtTime(r.at)), h("td", r.actor), h("td", h("code", r.operation)), h("td", { class: "muted" }, r.object_id)]))))),
        ]),
        h("section", { class: "card" }, [h("h3", "State"), h("pre", pretty(a.state))]),
      ];
    });
  },
};

const Task = {
  props: ["id"],
  setup(props) {
    const { view, load } = loader(() => api("GET", "/api/tasks/" + props.id));
    onMounted(load);
    const retry = async () => {
      const note = await promptBox("Retry this task?", "It continues in its existing run.", { placeholder: "Note for the agent (optional)", label: "Note", confirm: "Retry" });
      if (note !== null && await act("Retry", "POST", `/api/tasks/${props.id}/retry`, { note })) load();
    };
    const note = reactive({ text: "" });
    const models = reactive({ list: [], tiers: {} });
    onMounted(async () => { try { const m = await api("GET", "/api/models"); models.list = m.models; models.tiers = m.tiers; } catch { /* optional */ } });
    const choose = async (value) => {
      try {
        const res = await api("PUT", `/api/tasks/${props.id}/model`, { model: value });
        say(res.message);
        load();
      } catch (e) { say("Model not changed: " + e.message); load(); }
    };
    const tell = async (e) => {
      e.preventDefault();
      if (note.text.trim() && await act("Note", "POST", `/api/tasks/${props.id}/notes`, { text: note.text })) { note.text = ""; load(); }
    };
    return shell(() => {
      if (view.loading) return loading();
      if (view.error) return failed(view.error);
      const t = view.data;
      return [
        h("p", { class: "small" }, link("/a/" + t.assignment_id, "← Assignment")),
        pageHead(t.title, null, [status(t.status), h("span", { class: "muted small" }, `${t.attempts} attempt${t.attempts === 1 ? "" : "s"}`)]),
        h("section", { class: "card" }, [
          h("h3", "Objective"),
          h("pre", t.objective),
          t.result ? h("pre", pretty(t.result)) : null,
          t.status === "blocked" ? h("button", { class: "primary", onClick: retry }, "Retry") : null,
        ]),
        h("section", { class: "card" }, [
          h("h3", "Model"),
          h("div", { class: "row" }, [
            h("label", { for: "task-model" }, "Use"),
            h("select", { id: "task-model", value: t.model_override || "auto", onChange: (e) => choose(e.target.value) }, [
              h("option", { value: "auto" }, "automatic" + (t.model_tier ? ` (tier ${t.model_tier})` : "")),
              ...["fast", "default", "strong"].map((tier) => h("option", { value: tier }, `tier ${tier}` + (models.tiers[tier] ? ` (${models.tiers[tier]})` : ""))),
              ...models.list.map((m) => h("option", { value: m.id }, `${m.id} (${m.local ? "local" : "cloud"})`)),
            ]),
          ]),
          t.runs.map((r) => {
            let why = "";
            try { const c = r.chosen; why = c ? ` · chosen by ${c.source}${c.skipped && c.skipped.length ? "; skipped " + c.skipped.join("; ") : ""}` : ""; } catch { /* old run */ }
            return h("p", { class: "muted small" }, `run ${r.id}: ${r.model} (${r.status})${why}`);
          }),
        ]),
        t.status === "completed" || t.status === "cancelled" ? null : h("form", { class: "card", onSubmit: tell }, [
          h("label", { for: "note" }, "Message this task"),
          h("p", { class: "muted small" }, "Your words reach the task at its next step. They do not change what it is allowed to do."),
          h("textarea", { id: "note", rows: 2, value: note.text, onInput: (e) => { note.text = e.target.value; } }),
          h("div", { class: "row end" }, h("button", { class: "primary", type: "submit", disabled: !note.text.trim() }, "Send note")),
        ]),
        h("section", { class: "card" }, [
          h("h3", "Transcript"),
          t.transcript.map((m) => h("div", { class: "msg " + m.role }, [
            h("div", { class: "eyebrow" }, m.role + (m.tool_call_id ? " · " + m.tool_call_id : "")),
            m.content ? h("pre", m.content) : null,
            m.tool_calls ? h("pre", pretty(m.tool_calls)) : null,
          ])),
        ]),
      ];
    });
  },
};

const Inbox = {
  setup() {
    const { view, load } = loader(async () => {
      const [approvals, unresolved, notifications] = await Promise.all([
        api("GET", "/api/approvals"), api("GET", "/api/actions/unresolved"), api("GET", "/api/notifications"),
      ]);
      return { approvals, unresolved, notifications };
    });
    onMounted(load);
    const decide = async (a, approve) => {
      const verb = approve ? "approve" : "reject";
      if (!await confirmBox(`${cap(verb)} ${a.tool}?`, `${a.resource || ""}\nAction ${a.action_id}\nArguments sha256 ${a.arguments_sha256.slice(0, 16)}…`.trim(), { confirm: cap(verb), danger: !approve })) return;
      if (await act(verb, "POST", `/api/approvals/${a.id}/${verb}`)) load();
    };
    const resolve = async (u, happened) => {
      const note = await promptBox(happened ? "It happened" : "It did not happen", happened ? "How did you confirm it took effect?" : "How did you confirm it did not take effect?", { placeholder: "What you checked", label: "How you confirmed", confirm: "Record" });
      if (note !== null && await act("Resolve", "POST", `/api/actions/${u.id}/resolve`, { happened, note })) load();
    };
    return shell(() => {
      if (view.loading) return loading();
      if (view.error) return failed(view.error);
      const d = view.data;
      return [
        pageHead("Inbox", "Decisions only you can make."),
        sectionHead("Waiting for your approval", d.approvals.length),
        d.approvals.length ? d.approvals.map((a) => h("article", { class: "card" }, [
          h("h3", [a.tool, " ", h("span", { class: "muted" }, a.resource || "")]),
          h("p", { class: "muted" }, [`action ${a.action_id} · expires ${fmtTime(a.expires_at)} · `, link("/a/" + a.assignment_id, "assignment")]),
          h("pre", pretty(a.arguments)),
          h("p", { class: "muted" }, "Exactly these arguments (sha256 " + a.arguments_sha256 + ") will run; any change needs a new approval."),
          h("div", { class: "row" }, [h("button", { class: "primary", onClick: () => decide(a, true) }, "Approve"), h("button", { onClick: () => decide(a, false) }, "Reject")]),
        ])) : h("p", { class: "muted" }, "Nothing waiting."),
        sectionHead("Outcome unknown", d.unresolved.length),
        d.unresolved.length ? d.unresolved.map((u) => h("article", { class: "card" }, [
          h("h3", [u.tool, " ", h("span", { class: "muted" }, u.resource || "")]),
          h("p", { class: "muted" }, ["action ", u.id, " · ", link("/t/" + u.task_id, "task")]),
          h("pre", pretty(u.arguments)),
          u.detail ? h("pre", pretty(u.detail)) : null,
          h("p", "agentd could not tell whether this happened, so it will not retry it. Check the real system, then:"),
          h("div", { class: "row" }, [h("button", { onClick: () => resolve(u, true) }, "It happened"), h("button", { onClick: () => resolve(u, false) }, "It did not happen")]),
        ])) : h("p", { class: "muted" }, "None."),
        sectionHead("Undelivered notifications", d.notifications.length),
        d.notifications.length ? h("section", { class: "card" }, h("table", h("tbody", d.notifications.map((n) => h("tr", [
          h("td", status(n.severity)), h("td", [h("strong", n.title), h("div", n.body)]),
          h("td", { class: "muted" }, n.delivery_failed ? "failed: " + n.last_error : n.delivery_attempts ? "retrying: " + (n.last_error || "") : "waiting"),
        ]))))) : h("p", { class: "muted" }, "None."),
      ];
    });
  },
};

const Tools = {
  setup() {
    const { view, load } = loader(async () => {
      const [servers, tools] = await Promise.all([api("GET", "/api/mcp/servers"), api("GET", "/api/mcp/tools")]);
      state.servers = servers;
      return { servers, tools };
    });
    onMounted(load);
    const accept = async (p) => {
      if (!await confirmBox(`Accept ${p.server}__${p.tool}?`, `Exactly as shown, definition ${p.definition_hash.slice(0, 12)}.`, { confirm: "Accept" })) return;
      if (await act("Accept", "POST", `/api/mcp/servers/${encodeURIComponent(p.server)}/tools/${encodeURIComponent(p.tool)}/accept`, { definition_hash: p.definition_hash })) load();
    };
    const restart = async (s) => {
      if (await act("Restart", "POST", `/api/mcp/servers/${encodeURIComponent(s.server)}/restart`)) load();
    };
    const schema = (raw) => { try { return pretty(JSON.parse(JSON.stringify(raw)).inputSchema); } catch { return ""; } };
    return shell(() => {
      if (view.loading) return loading();
      if (view.error) return failed(view.error);
      const d = view.data;
      const pending = d.tools.filter((p) => p.status === "new" || p.status === "changed");
      return [
        pageHead("Tools", "Services agentd can use, and tool definitions waiting for your review."),
        sectionHead("MCP servers", d.servers.length),
        d.servers.length ? h("section", { class: "card scroll-x" }, h("table", h("tbody", d.servers.map((s) => h("tr", [
          h("td", [h("strong", s.server), h("div", { class: "muted" }, s.transport + (s.scopes ? " · scopes " + s.scopes : ""))]),
          h("td", status(s.state)),
          h("td", { class: "muted" }, Object.entries(s.tools).map(([k, v]) => v + " " + k).join(", ")),
          h("td", { class: "muted" }, [s.last_error || "", s.next_retry_at ? " · retry " + fmtTime(s.next_retry_at) : ""]),
          h("td", s.state === "failed" ? h("button", { onClick: () => restart(s) }, "Restart") : null),
        ]))))) : h("p", { class: "muted" }, "No MCP server has run yet. Start agentd with -mcp-config."),
        sectionHead("Tools awaiting review", pending.length),
        pending.length ? pending.map((p) => h("article", { class: "card" }, [
          h("h3", [p.server + "__" + p.tool, " ", status(p.status)]),
          h("p", { class: "muted" }, "Text is shown with hidden characters escaped. A tool runs only after you accept this exact definition."),
          h("h4", "Description"), h("pre", p.description_visible || "(none)"),
          h("h4", "Input schema"), h("pre", schema(p.definition)),
          p.status === "changed" ? [h("h4", "Previously accepted description"), h("pre", p.accepted_description_visible || "(none)"),
            h("h4", "Previously accepted schema"), h("pre", schema(p.accepted_definition))] : null,
          h("div", { class: "row" }, h("button", { class: "primary", onClick: () => accept(p) }, "Accept this definition")),
        ])) : h("p", { class: "muted" }, "Nothing to review."),
      ];
    });
  },
};

const Activity = {
  setup() {
    const feed = reactive({ rows: [], connected: false, error: "" });
    const seen = new Set();
    let source;
    let retry = 1000;
    let timer;
    const connect = () => {
      source = new EventSource("/api/events/stream?after=" + Math.max(0, lastID() - 50));
      source.onopen = () => { feed.connected = true; retry = 1000; };
      source.addEventListener("activity", (e) => {
        const row = JSON.parse(e.data);
        if (seen.has(row.id)) return;
        seen.add(row.id);
        feed.rows.unshift(row);
        if (feed.rows.length > 200) feed.rows.pop();
      });
      source.onerror = () => {
        feed.connected = false;
        source.close();
        clearTimeout(timer);
        timer = setTimeout(connect, retry);
        retry = Math.min(retry * 2, 30000);
      };
    };
    const lastID = () => (feed.rows.length ? feed.rows[0].id : 0);
    onMounted(connect);
    onUnmounted(() => { clearTimeout(timer); if (source) source.close(); });
    return shell(() => [
      pageHead("Activity", null, h("span", { class: "sys " + (feed.connected ? "on" : "off"), role: "status" },
        [h("span", { class: "dot" }), feed.connected ? "Live" : "Reconnecting… showing what arrived so far"])),
      h("section", { class: "card scroll-x" }, feed.rows.length
        ? h("table", h("tbody", feed.rows.map((r) => h("tr", { key: r.id }, [
            h("td", { class: "muted" }, fmtTime(r.at)), h("td", r.actor), h("td", r.operation),
            h("td", r.assignment_id ? link("/a/" + r.assignment_id, r.object_id || r.assignment_id) : h("span", { class: "muted" }, r.object_id)),
          ]))))
        : h("p", { class: "muted" }, "Waiting for activity…")),
    ]);
  },
};

const Chat = makeChat({ state, api, say, shell, refreshShell, mark });

const App = {
  setup() {
    onMounted(async () => {
      try {
        const res = await fetch("/ui/session", { credentials: "same-origin" });
        if (res.ok) {
          state.csrf = (await res.json()).csrf;
          state.signedIn = true;
          refreshShell();
        } else {
          state.signedIn = false;
        }
      } catch {
        state.signedIn = false;
      }
    });
    return () => {
      if (state.signedIn !== true) return h(shell(() => null));
      const r = state.route;
      if (r.startsWith("/a/")) return h(Assignment, { id: decodeURIComponent(r.slice(3)), key: r });
      if (r.startsWith("/t/")) return h(Task, { id: decodeURIComponent(r.slice(3)), key: r });
      if (r === "/inbox") return h(Inbox, { key: r });
      if (r === "/activity") return h(Activity, { key: r });
      if (r === "/tools") return h(Tools, { key: r });
      if (r === "/work") return h(Assignments, { key: r });
      const about = r.startsWith("/?about=") ? decodeURIComponent(r.slice(8)) : "";
      return h(Chat, { key: "/" + about, about });
    };
  },
};

createApp(App).mount("#app");
