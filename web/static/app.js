// Expense approvals UI. Polls the session-scoped API, updates the DOM by key so
// nothing re-animates, and connects each live step to its line of workflow code.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const state = {
    config: null,
    expenses: [],
    placeholders: new Map(), // submitted but not yet visible in Temporal's list
    selected: null,
    detail: null,
    online: true,
    anchors: {},
  };

  // ---------- helpers ----------
  async function api(path, options = {}) {
    const res = await fetch(path, {
      credentials: "same-origin",
      headers: options.body ? { "Content-Type": "application/json" } : undefined,
      ...options,
      body: options.body ? JSON.stringify(options.body) : undefined,
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`);
    return data;
  }

  const money = (cents) => (cents / 100).toLocaleString("en-US", { style: "currency", currency: "USD" });
  const clock = (iso) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  function span(ms) {
    const s = Math.max(0, Math.round(ms / 1000));
    return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  }
  function el(tag, cls, text) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  let toastTimer;
  function toast(msg, isError = false) {
    const t = $("toast");
    t.textContent = msg;
    t.classList.toggle("error", isError);
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => (t.hidden = true), isError ? 5000 : 3000);
  }

  // The live timer step for an expense: the escalation or expiry countdown.
  const openTimer = (e) => [...(e.steps || [])].reverse().find((s) => s.key.startsWith("wait") && s.until);

  // ---------- requests list ----------
  function allExpenses() {
    const ids = new Set(state.expenses.map((e) => e.id));
    for (const id of state.placeholders.keys()) if (ids.has(id)) state.placeholders.delete(id);
    return [...state.placeholders.values(), ...state.expenses];
  }

  function renderList() {
    const list = $("requests");
    const items = allExpenses();
    $("requests-empty").hidden = items.length > 0;
    const keep = new Set();
    items.forEach((e, i) => {
      keep.add(e.id);
      let li = list.querySelector(`[data-id="${e.id}"]`);
      if (!li) {
        li = el("li", "request");
        li.dataset.id = e.id;
        li.tabIndex = 0;
        li.append(el("span", "desc"), el("span", "amount"));
        const meta = el("span", "muted");
        meta.append(el("span", "status"), el("span", "when"));
        li.append(meta);
        li.addEventListener("click", () => select(e.id));
        li.addEventListener("keydown", (ev) => ev.key === "Enter" && select(e.id));
      }
      if (list.children[i] !== li) list.insertBefore(li, list.children[i] || null);
      li.querySelector(".desc").textContent = e.description;
      li.querySelector(".amount").textContent = money(e.amountCents);
      const status = li.querySelector(".status");
      status.textContent = e.label;
      status.className = `status tone-${e.tone}`;
      li.querySelector(".when").textContent = clock(e.submittedAt);
      li.setAttribute("aria-current", String(e.id === state.selected));
    });
    [...list.children].forEach((li) => keep.has(li.dataset.id) || li.remove());
  }

  // ---------- approver inbox ----------
  function renderInbox() {
    const inbox = $("inbox");
    const waiting = state.expenses.filter((e) => e.awaitingDecision);
    $("inbox-empty").hidden = waiting.length > 0;
    const keep = new Set();
    for (const e of waiting) {
      keep.add(e.id);
      const escalated = e.canDecide.length > 1;
      const actingAs = escalated ? state.config.escalationApprover : state.config.approver;
      let li = inbox.querySelector(`[data-id="${e.id}"]`);
      if (!li) {
        li = el("li", "inbox-item");
        li.dataset.id = e.id;
        const top = el("div", "inbox-top");
        top.append(el("span", "desc", e.description), el("span", "amount", money(e.amountCents)));
        const actions = el("div", "inbox-actions");
        const comment = el("input");
        comment.placeholder = "Comment (optional)";
        comment.maxLength = 140;
        const approve = el("button", "button primary green small", "Approve");
        const reject = el("button", "button secondary reject small", "Reject");
        approve.type = reject.type = "button";
        approve.addEventListener("click", () => decide(e.id, true, li));
        reject.addEventListener("click", () => decide(e.id, false, li));
        actions.append(comment, approve, reject);
        li.append(top, el("span", "muted from"), el("span", "muted timer tone-waiting"), actions);
        inbox.append(li);
      }
      li.classList.toggle("escalated", escalated);
      li.dataset.as = escalated ? "escalation" : "approver";
      li.querySelector(".from").textContent = `From ${e.employee} · ${e.category} · deciding as ${actingAs}`;
      const timer = openTimer(e);
      const t = li.querySelector(".timer");
      t.dataset.until = timer ? timer.until : "";
      t.dataset.prefix = escalated ? "Escalated · expires in " : "Escalates in ";
    }
    [...inbox.children].forEach((li) => keep.has(li.dataset.id) || li.remove());
  }

  async function decide(id, approved, li) {
    li.querySelectorAll("button").forEach((b) => (b.disabled = true));
    try {
      await api(`/api/expenses/${id}/decision`, {
        method: "POST",
        body: { approved, as: li.dataset.as, comment: li.querySelector("input").value },
      });
      toast(`${approved ? "Approval" : "Rejection"} recorded by Temporal`);
      select(id);
      refreshList();
    } catch (err) {
      toast(err.message, true);
      li.querySelectorAll("button").forEach((b) => (b.disabled = false));
    }
  }

  // ---------- journey ----------
  function renderJourney() {
    const d = state.detail;
    $("journey-empty").hidden = !!d;
    $("journey-body").hidden = !d;
    if (!d) return;
    const e = d.expense;
    $("j-title").textContent = `${e.description} · ${money(e.amountCents)}`;
    $("j-meta").textContent = `${e.category} · submitted by ${e.employee} at ${clock(e.submittedAt)}`;
    const pill = $("j-status");
    pill.textContent = e.label;
    pill.className = `status-pill tone-${e.tone}`;
    $("j-wfid").textContent = e.workflowId;
    $("j-wfid").title = e.workflowId;
    $("j-link").href = d.temporalUrl;

    const banner = $("j-worker");
    banner.hidden = !e.waitingForWorkerSince;
    banner.dataset.since = e.waitingForWorkerSince || "";
    banner.dataset.decision = e.decisionRecorded ? "1" : "";

    const list = $("j-steps");
    if (list.dataset.for !== e.id) {
      list.replaceChildren();
      list.dataset.for = e.id;
    }
    e.steps.forEach((s, i) => {
      let li = list.children[i];
      if (!li) {
        li = el("li", "step enter");
        li.append(el("span", "dot"), el("span", "step-label"), el("span", "step-time"), el("span", "step-detail"), el("span", "step-countdown"));
        list.append(li);
      }
      li.dataset.state = s.state;
      li.querySelector(".dot").textContent = { done: "✓", recovered: "↺", failed: "!" }[s.state] || "";
      li.querySelector(".step-label").textContent = s.label + (s.attempt > 1 && s.state !== "done" ? ` · attempt ${s.attempt}` : "");
      // Waits and worker gaps span other steps, so show when they started and ended.
      li.querySelector(".step-time").textContent = clock(s.at) + (s.endedAt ? `\nto ${clock(s.endedAt)}` : "");
      li.querySelector(".step-detail").textContent = s.detail || "";
      const cd = li.querySelector(".step-countdown");
      cd.dataset.until = s.until || "";
      cd.dataset.prefix = s.state === "retrying" ? "Next attempt in " : s.key === "wait" ? "Escalates in " : "Expires in ";
    });
    while (list.children.length > e.steps.length) list.lastChild.remove();
    highlightCode(e);
    tick();
  }

  // ---------- code panel ----------
  function renderSource(src) {
    const lines = src.replace(/\n$/, "").split("\n");
    // Same token palette as the slide deck: keywords, function names, strings,
    // constants and types, and comments each get their own color.
    const token = /("(?:[^"\\]|\\.)*")|(\/\/.*$)|\b(func|return|if|else|for|range|switch|case|default|var|const|type|struct|package|import|continue|break|defer|go)\b|\b(nil|true|false|bool|string|error|int|int32|int64|float64|any)\b|\b(\d+(?:\.\d+)?)\b|\b([A-Za-z_]\w*)(?=\()/g;
    const esc = (s) => s.replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" })[c]);
    const html = lines.map((line) => {
      let out = "", last = 0, m;
      token.lastIndex = 0;
      while ((m = token.exec(line))) {
        out += esc(line.slice(last, m.index));
        const cls = m[1] ? "s" : m[2] ? "c" : m[3] ? "k" : m[6] ? "f" : "t";
        out += `<span class="${cls}">${esc(m[0])}</span>`;
        last = token.lastIndex;
      }
      return `<span class="ln">${out + esc(line.slice(last))}</span>`;
    });
    $("code").innerHTML = html.join("");

    // Anchors are found in the running source, so they can't drift from it.
    const find = (needle, nth = 1) => {
      let seen = 0;
      for (let i = 0; i < lines.length; i++) if (lines[i].includes(needle) && ++seen === nth) return i;
      return -1;
    };
    const retry = [find("RetryPolicy:"), find("InitialInterval"), find("BackoffCoefficient"), find("MaximumInterval")];
    state.anchors = {
      submit: [find("func ExpenseApproval")],
      policy: [find("a.CheckPolicy")],
      "notify-approver": [find("a.NotifyApprover", 1)],
      wait: [find("awaitDecision(ctx, req.EscalateAfter")],
      escalate: [find("a.NotifyApprover", 2)],
      "wait-escalated": [find("awaitDecision(ctx, req.ExpireAfter")],
      decision: [find("decisions.Receive"), find("status.Decision = &d")],
      pay: [find("a.PayReimbursement")],
      "pay-retry": [find("a.PayReimbursement"), ...retry],
      "notify-employee": [find("a.NotifyEmployee")],
      complete: [find("return result, nil")],
    };
    if (state.detail) highlightCode(state.detail.expense);
  }

  let lastHighlight = "";
  function highlightCode(e) {
    const steps = e.steps || [];
    if (!steps.length || !Object.keys(state.anchors).length) return;
    const open = steps.filter((s) => s.state !== "done" && s.state !== "recovered");
    let cur = open.length ? open[open.length - 1] : steps[steps.length - 1];
    if (cur.key.startsWith("wait") && steps[steps.length - 1].key === "decision") cur = steps[steps.length - 1];
    const key = cur.key === "pay" && cur.state === "retrying" ? "pay-retry" : cur.key;
    const sig = `${e.id}:${key}:${cur.state}`;
    if (sig === lastHighlight) return;
    lastHighlight = sig;

    const lines = $("code").children;
    for (const ln of $("code").querySelectorAll(".hl")) ln.className = "ln";
    const idx = (state.anchors[key] || []).filter((i) => i >= 0);
    const extra = cur.state === "retrying" ? " retry" : cur.state === "waiting" ? " wait" : "";
    idx.forEach((i) => (lines[i].className = "ln hl" + extra));
    if (idx.length) {
      const code = $("code");
      const target = lines[idx[0]];
      code.scrollTo({ top: target.offsetTop - code.clientHeight / 3, behavior: "smooth" });
    }
  }

  // ---------- timers (client-side countdowns from server deadlines) ----------
  function tick() {
    const now = Date.now();
    document.querySelectorAll("[data-until]").forEach((node) => {
      const until = node.dataset.until;
      if (!until) { node.textContent = ""; return; }
      const left = new Date(until) - now;
      node.textContent = left > 0 ? node.dataset.prefix + span(left) : "Due now. Temporal fires the timer on time";
    });
    const banner = $("j-worker");
    if (!banner.hidden && banner.dataset.since) {
      const held = span(now - new Date(banner.dataset.since));
      const what = banner.dataset.decision ? "The request, its timers, and the decision are" : "The request and its timers are";
      $("j-worker-text").textContent =
        `The next step has been ready for ${held}. ${what} safe in Temporal and continue as soon as a worker polls task queue “${state.config.taskQueue}”.`;
    }
  }

  // ---------- data loops ----------
  async function refreshList() {
    try {
      const data = await api("/api/expenses");
      state.expenses = data.expenses;
      setOnline(true);
      if (!state.selected && state.expenses.length) select(state.expenses[0].id);
    } catch {
      setOnline(false);
    }
    renderList();
    renderInbox();
  }

  async function refreshDetail() {
    if (!state.selected) return;
    const id = state.selected;
    try {
      const data = await api(`/api/expenses/${id}`);
      if (state.selected !== id) return;
      state.detail = data;
      renderJourney();
    } catch (err) {
      // A just-submitted expense is readable immediately; anything else is a connectivity blip.
      if (!/not found/i.test(err.message)) setOnline(false);
    }
  }

  function setOnline(ok) {
    state.online = ok;
    $("list-status").textContent = ok ? "" : "Reconnecting to Temporal…";
  }

  function loop(fn, ms) {
    const run = async () => { await fn(); setTimeout(run, ms); };
    run();
  }

  function select(id) {
    if (state.selected !== id) {
      state.selected = id;
      state.detail = null;
      lastHighlight = "";
      renderJourney();
    }
    renderList();
    refreshDetail();
  }

  // ---------- form ----------
  function wireForm() {
    const form = $("expense-form");
    document.querySelectorAll(".chip").forEach((chip) =>
      chip.addEventListener("click", () => {
        form.description.value = chip.dataset.desc;
        form.category.value = chip.dataset.cat;
        form.amount.value = chip.dataset.amount;
        form.paymentFailures.value = chip.dataset.fail;
        $("submit").focus();
      }));

    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const amountCents = Math.round(parseFloat(form.amount.value) * 100);
      const body = {
        description: form.description.value,
        category: form.category.value,
        amountCents,
        paymentFailures: Number(form.paymentFailures.value),
      };
      const btn = $("submit");
      btn.disabled = true;
      try {
        const { id } = await api("/api/expenses", { method: "POST", body });
        state.placeholders.set(id, {
          id, description: body.description, category: body.category, amountCents,
          employee: state.config.employee, submittedAt: new Date().toISOString(),
          label: "Submitted", tone: "progress", steps: [],
        });
        form.reset();
        toast("Expense submitted. Temporal is running the approval");
        select(id);
      } catch (err) {
        toast(err.message, true);
      } finally {
        btn.disabled = false;
      }
    });

    $("reset").addEventListener("click", async () => {
      try {
        await api("/api/session/reset", { method: "POST" });
        Object.assign(state, { expenses: [], selected: null, detail: null });
        state.placeholders.clear();
        renderList(); renderInbox(); renderJourney();
        toast("New session started. Earlier requests carry on without you");
      } catch (err) {
        toast(err.message, true);
      }
    });
  }

  // ---------- start ----------
  async function start() {
    try {
      state.config = await api("/api/config");
    } catch {
      setTimeout(start, 2000);
      return;
    }
    const c = state.config;
    $("employee-name").textContent = c.employee;
    $("approver-name").textContent = c.approver;
    $("queue").textContent = `task queue · ${c.taskQueue}`;
    $("temporal-ui").href = `${c.temporalUI}/workflows`;
    fetch("/api/source").then((r) => r.text()).then(renderSource).catch(() => {});
    loop(refreshList, 1500);
    loop(refreshDetail, 1000);
    setInterval(tick, 250);
  }

  if (window.Starfield) Starfield.start($("stars"));
  wireForm();
  start();
})();
