import { createApp, h, reactive, onMounted, onUnmounted, watch } from "/vue.js";
import { makeChat } from "/chat.js";

const state = reactive({
  csrf: null,
  signedIn: null,
  route: location.hash.slice(1) || "/",
  system: { halted: false },
  counts: { approvals: 0, unresolved: 0, questions: 0 },
  servers: [],
  toast: "",
});

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

const fmtTime = (t) => (t ? new Date(t).toLocaleString() : "");
const money = (n) => "$" + Number(n || 0).toFixed(2);
const pretty = (v) => { try { return JSON.stringify(typeof v === "string" ? JSON.parse(v) : v, null, 2); } catch { return String(v); } };
const status = (s) => h("span", { class: "status " + s }, s.replace(/_/g, " "));
const link = (to, text) => h("a", { href: "#" + to }, text);

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

function shell(content) {
  return () => {
    if (state.signedIn === false) return h(Login);
    if (state.signedIn === null) return h("main", h("p", { class: "muted" }, "Loading…"));
    const items = [["/", "Chat"], ["/work", "Work"], ["/inbox", "Inbox"], ["/tools", "Tools"], ["/activity", "Activity"]];
    const waiting = state.counts.approvals + state.counts.unresolved + state.counts.questions;
    const troubled = state.servers.filter((s) => s.state === "needs_auth" || s.state === "failed");
    const reviews = state.servers.filter((s) => s.state === "needs_review").length;
    return h("div", [
      state.system.halted
        ? h("div", { class: "halted", role: "alert" }, [
            h("strong", "Emergency stop is on."),
            h("span", state.system.reason || ""),
            h("button", { onClick: () => confirm("Lift the emergency stop and let Baseet work again?") && act("Resume", "POST", "/api/system/resume") }, "Lift stop"),
          ])
        : null,
      h("header", [
        h("h1", "Baseet"),
        h("nav", { "aria-label": "Main" }, items.map(([to, text]) =>
          h("a", { href: "#" + to, class: state.route === to ? "active" : "", "aria-current": state.route === to ? "page" : null }, [
            text, (to === "/inbox" || to === "/") && waiting ? [" ", h("span", { class: "badge", "aria-label": waiting + " waiting" }, String(waiting))] : null,
            to === "/tools" && troubled.length + reviews ? [" ", h("span", { class: "badge", "aria-label": (troubled.length + reviews) + " need attention" }, String(troubled.length + reviews))] : null,
          ]))),
        state.system.halted ? null : h("button", {
          class: "danger",
          onClick: () => {
            const reason = prompt("Emergency stop: no task starts, no tool runs, no model call is made. Reason?");
            if (reason !== null) act("Emergency stop", "POST", "/api/system/emergency-stop", { reason });
          },
        }, "Emergency stop"),
        h("button", { onClick: async () => { await api("DELETE", "/ui/session").catch(() => {}); state.signedIn = false; state.csrf = null; } }, "Sign out"),
      ]),
      troubled.length && state.route !== "/tools"
        ? h("div", { class: "card banner", role: "alert" }, [
            h("strong", "Tools unavailable: "),
            troubled.map((s) => s.server + " " + s.state.replace("_", " ")).join(", "), " · ", link("/tools", "details"),
          ])
        : null,
      h("main", content()),
      h("div", { class: "sr-only", role: "status", "aria-live": "polite" }, state.toast),
      state.toast ? h("div", { class: "toast", "aria-hidden": "true" }, state.toast) : null,
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
      h("h2", "Sign in to Baseet"),
      h("p", { class: "muted" }, "Paste the API token from the api-token file. It is sent once; the browser keeps only a session cookie it cannot read."),
      h("label", { for: "token" }, "API token"),
      h("input", { id: "token", type: "password", autocomplete: "off", value: form.token, onInput: (e) => { form.token = e.target.value; } }),
      form.error ? h("p", { role: "alert", style: "color: var(--bad)" }, form.error) : null,
      h("p", h("button", { class: "primary", type: "submit", disabled: form.busy || !form.token }, "Sign in")),
    ]));
  },
};

const Assignments = {
  setup() {
    const { view, load } = loader(() => api("GET", "/api/assignments"));
    onMounted(load);
    return shell(() => {
      if (view.loading) return h("p", { class: "muted" }, "Loading…");
      if (view.error) return h("p", { role: "alert" }, view.error);
      if (!view.data.length) return h("p", ["Nothing assigned yet. ", link("/", "Ask for something in the chat"), " or create one with agentd create."]);
      return h("div", { class: "grid" }, view.data.map((a) => h("article", { class: "card" }, [
        h("h2", link("/a/" + a.id, a.name)),
        h("div", { class: "row" }, [status(a.status), h("span", { class: "muted" }, a.kind)]),
        a.status_reason ? h("p", { class: "muted" }, a.status_reason) : null,
        h("p", ["Next: ", a.next.what, a.next.at ? h("span", { class: "muted" }, " · " + fmtTime(a.next.at)) : null]),
        h("p", { class: "muted" }, [
          "Spend ", money(a.spent_usd), a.budget_usd ? " of " + money(a.budget_usd) : "",
          a.pending_approvals ? " · " + a.pending_approvals + " approval(s)" : "",
          a.blocked_tasks ? " · " + a.blocked_tasks + " blocked" : "",
          a.overdue_obligations ? " · " + a.overdue_obligations + " overdue" : "",
        ]),
      ])));
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
      if (action !== "pause" && action !== "resume" && !confirm(`${action[0].toUpperCase() + action.slice(1)} this assignment? This cannot be undone.`)) return;
      if (await act(action, "POST", `/api/assignments/${props.id}/${action}`)) load();
    };
    const confirmCriterion = async (c) => {
      const note = prompt(`Confirm "${c.description}" yourself? Add a note (optional):`);
      if (note !== null && await act("Confirm", "POST", `/api/assignments/${props.id}/criteria/${encodeURIComponent(c.id)}/confirm`, { note })) load();
    };
    return shell(() => {
      if (view.loading) return h("p", { class: "muted" }, "Loading…");
      if (view.error) return h("p", { role: "alert" }, view.error);
      const a = view.data;
      const actions = (lifecycle[a.kind] || {})[a.status] || [];
      return [
        h("section", { class: "card" }, [
          h("h2", a.name),
          h("div", { class: "row" }, [status(a.status), h("span", { class: "muted" }, a.kind), a.status_reason ? h("span", { class: "muted" }, a.status_reason) : null]),
          h("p", a.objective),
          h("p", ["Next: ", h("strong", a.next.what)]),
          h("p", link("/?about=" + a.id, "Ask about this")),
          (a.policy_warnings || []).map((w) => h("p", { class: "status pending", role: "note" }, "Policy: " + w)),
          h("p", { class: "muted" }, ["Spend ", money(a.spent_usd), a.budget_usd ? ` of ${money(a.budget_usd)}${a.budget_period ? " per " + a.budget_period : ""}` : "",
            a.reserved_usd ? ` (+${money(a.reserved_usd)} held)` : ""]),
          h("div", { class: "row" }, actions.map((x) => h("button", { class: x === "pause" || x === "resume" ? "" : "danger", onClick: () => run(x) }, x[0].toUpperCase() + x.slice(1)))),
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
          h("table", h("tbody", a.recent.map((r) => h("tr", [h("td", { class: "muted" }, fmtTime(r.at)), h("td", r.actor), h("td", r.operation), h("td", { class: "muted" }, r.object_id)])))),
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
      const note = prompt("Continue this blocked task in its existing run. Note for the agent (optional):");
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
      if (view.loading) return h("p", { class: "muted" }, "Loading…");
      if (view.error) return h("p", { role: "alert" }, view.error);
      const t = view.data;
      return [
        h("section", { class: "card" }, [
          h("h2", t.title),
          h("div", { class: "row" }, [status(t.status), h("span", { class: "muted" }, `attempts ${t.attempts}`), link("/a/" + t.assignment_id, "assignment")]),
          h("pre", t.objective),
          t.result ? h("pre", pretty(t.result)) : null,
          t.status === "blocked" ? h("button", { onClick: retry }, "Retry") : null,
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
          h("p", h("button", { type: "submit", disabled: !note.text.trim() }, "Send note")),
        ]),
        h("section", { class: "card" }, [
          h("h3", "Transcript"),
          t.transcript.map((m) => h("div", { class: "msg " + m.role }, [
            h("div", { class: "muted" }, m.role + (m.tool_call_id ? " · " + m.tool_call_id : "")),
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
      if (!confirm(`${verb[0].toUpperCase() + verb.slice(1)} action ${a.action_id}?\n\n${a.tool} ${a.resource || ""}\narguments sha256 ${a.arguments_sha256.slice(0, 16)}…`)) return;
      if (await act(verb, "POST", `/api/approvals/${a.id}/${verb}`)) load();
    };
    const resolve = async (u, happened) => {
      const note = prompt(happened ? "How did you confirm it took effect?" : "How did you confirm it did not take effect?");
      if (note !== null && await act("Resolve", "POST", `/api/actions/${u.id}/resolve`, { happened, note })) load();
    };
    return shell(() => {
      if (view.loading) return h("p", { class: "muted" }, "Loading…");
      if (view.error) return h("p", { role: "alert" }, view.error);
      const d = view.data;
      return [
        h("h2", "Waiting for your approval"),
        d.approvals.length ? d.approvals.map((a) => h("article", { class: "card" }, [
          h("h3", [a.tool, " ", h("span", { class: "muted" }, a.resource || "")]),
          h("p", { class: "muted" }, [`action ${a.action_id} · expires ${fmtTime(a.expires_at)} · `, link("/a/" + a.assignment_id, "assignment")]),
          h("pre", pretty(a.arguments)),
          h("p", { class: "muted" }, "Exactly these arguments (sha256 " + a.arguments_sha256 + ") will run; any change needs a new approval."),
          h("div", { class: "row" }, [h("button", { class: "primary", onClick: () => decide(a, true) }, "Approve"), h("button", { onClick: () => decide(a, false) }, "Reject")]),
        ])) : h("p", { class: "muted" }, "Nothing waiting."),
        h("h2", "Outcome unknown"),
        d.unresolved.length ? d.unresolved.map((u) => h("article", { class: "card" }, [
          h("h3", [u.tool, " ", h("span", { class: "muted" }, u.resource || "")]),
          h("p", { class: "muted" }, ["action ", u.id, " · ", link("/t/" + u.task_id, "task")]),
          h("pre", pretty(u.arguments)),
          u.detail ? h("pre", pretty(u.detail)) : null,
          h("p", "Baseet could not tell whether this happened, so it will not retry it. Check the real system, then:"),
          h("div", { class: "row" }, [h("button", { onClick: () => resolve(u, true) }, "It happened"), h("button", { onClick: () => resolve(u, false) }, "It did not happen")]),
        ])) : h("p", { class: "muted" }, "None."),
        h("h2", "Undelivered notifications"),
        d.notifications.length ? h("table", h("tbody", d.notifications.map((n) => h("tr", [
          h("td", status(n.severity)), h("td", [h("strong", n.title), h("div", n.body)]),
          h("td", { class: "muted" }, n.delivery_failed ? "failed: " + n.last_error : n.delivery_attempts ? "retrying: " + (n.last_error || "") : "waiting"),
        ])))) : h("p", { class: "muted" }, "None."),
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
      if (!confirm(`Accept ${p.server}__${p.tool} exactly as shown (definition ${p.definition_hash.slice(0, 12)})?`)) return;
      if (await act("Accept", "POST", `/api/mcp/servers/${encodeURIComponent(p.server)}/tools/${encodeURIComponent(p.tool)}/accept`, { definition_hash: p.definition_hash })) load();
    };
    const restart = async (s) => {
      if (await act("Restart", "POST", `/api/mcp/servers/${encodeURIComponent(s.server)}/restart`)) load();
    };
    const schema = (raw) => { try { return pretty(JSON.parse(JSON.stringify(raw)).inputSchema); } catch { return ""; } };
    return shell(() => {
      if (view.loading) return h("p", { class: "muted" }, "Loading…");
      if (view.error) return h("p", { role: "alert" }, view.error);
      const d = view.data;
      const pending = d.tools.filter((p) => p.status === "new" || p.status === "changed");
      return [
        h("h2", "MCP servers"),
        d.servers.length ? h("section", { class: "card" }, h("table", h("tbody", d.servers.map((s) => h("tr", [
          h("td", [h("strong", s.server), h("div", { class: "muted" }, s.transport + (s.scopes ? " · scopes " + s.scopes : ""))]),
          h("td", status(s.state)),
          h("td", { class: "muted" }, Object.entries(s.tools).map(([k, v]) => v + " " + k).join(", ")),
          h("td", { class: "muted" }, [s.last_error || "", s.next_retry_at ? " · retry " + fmtTime(s.next_retry_at) : ""]),
          h("td", s.state === "failed" ? h("button", { onClick: () => restart(s) }, "Restart") : null),
        ]))))) : h("p", { class: "muted" }, "No MCP server has run yet. Start agentd with -mcp-config."),
        h("h2", "Tools awaiting review"),
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
      h("div", { class: "row" }, [h("h2", "Activity"), h("span", { class: "muted", role: "status" }, feed.connected ? "live" : "reconnecting… (showing what arrived so far)")]),
      h("section", { class: "card" }, feed.rows.length
        ? h("table", h("tbody", feed.rows.map((r) => h("tr", { key: r.id }, [
            h("td", { class: "muted" }, fmtTime(r.at)), h("td", r.actor), h("td", r.operation),
            h("td", r.assignment_id ? link("/a/" + r.assignment_id, r.object_id || r.assignment_id) : h("span", { class: "muted" }, r.object_id)),
          ]))))
        : h("p", { class: "muted" }, "Waiting for activity…")),
    ]);
  },
};

const Chat = makeChat({ state, api, say, shell, refreshShell });

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
