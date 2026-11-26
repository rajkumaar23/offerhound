/* offerhound UI — no frameworks, desktop-first responsive grid. The server
   is a dumb store: this page only reads. A wallet dropdown switches users. */
const $ = (s) => document.querySelector(s);
const state = {
  q: "", channel: "", status: "", bank: "", timer: null,
  user: localStorage.getItem("oh-user") || "default",
};

function api(path, params = {}) {
  const qs = new URLSearchParams(params);
  if (state.user) qs.set("user", state.user);
  const sep = path.includes("?") ? "&" : "?";
  return fetch(`${path}${qs.toString() ? sep + qs : ""}`).then((r) => {
    if (!r.ok) throw new Error(r.status + " " + path);
    return r.json();
  });
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"]/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function safeURL(u) {
  // href injection guard: esc() does not stop javascript:/data: schemes.
  try {
    const x = new URL(u, location.href);
    return x.protocol === "https:" || x.protocol === "http:" ? x.href : "";
  } catch (e) { return ""; }
}

function daysLeft(iso) {
  if (!iso) return null;
  return Math.floor((new Date(iso + "T23:59:59Z") - Date.now()) / 86400000);
}

/* simpleicons.org slug heuristic: "Whole Foods In-Store" -> wholefoods.
   404s self-remove; unknown brands just show text. */
function merchantIcon(m) {
  // Strip noise words to a FIXPOINT: one pass can re-form a stripped word
  // from surrounding characters ("inin-store" + leftover "nline" ->
  // "online"), which is exactly what single-pass sanitization misses.
  let slug = String(m || "").toLowerCase();
  let prev;
  do {
    prev = slug;
    slug = slug.replace(/\b(in[- ]store|online|in[- ]app|store|official)\b/g, "");
  } while (slug !== prev);
  slug = slug.replace(/[^a-z0-9]/g, "");
  if (!slug || slug.length < 3) return "";
  return `<img class="micon" src="https://cdn.simpleicons.org/${slug}" alt="" loading="lazy" onerror="this.remove()">`;
}

function groupOffers(offers) {
  // Same merchant+reward on two cards = ONE offer shown on two card chips.
  // Grouping is display-only; underlying rows (per-card status/notes) are
  // kept for the detail sheet.
  const groups = [];
  const idx = new Map();
  for (const o of offers) {
    const k = [o.bank, o.merchant, o.title, o.rewardText, o.channel].join("\u0000");
    let g = idx.get(k);
    if (!g) {
      g = { bank: o.bank, merchant: o.merchant, title: o.title, rewardText: o.rewardText,
            channel: o.channel, description: o.description, cards: [] };
      idx.set(k, g);
      groups.push(g);
    }
    g.cards.push(o);
  }
  return groups;
}

async function renderResults() {
  let offers = [];
  try {
    offers = await api("/api/search", {
      q: state.q, channel: state.channel, status: state.status, bank: state.bank,
      limit: 5000,
    });
  } catch (e) { console.error(e); }
  const groups = groupOffers(offers);
  $("#meta").textContent = offers.length
    ? `${groups.length} offer${groups.length === 1 ? "" : "s"}` +
      (groups.length !== offers.length ? ` (on ${offers.length} card listings)` : "")
    : "";
  if (!groups.length) {
    $("#results").innerHTML = `<div class="empty"><img class="empty-mascot" src="/assets/mascot.png" alt="offerhound">
      No matching offers in this wallet.<br>
      Try a looser term, another wallet, or run a sync from your client.</div>`;
    return;
  }
  $("#results").innerHTML = groups.map((g, i) => {
    const allAdded = g.cards.every((c) => c.status === "enrolled");
    const someAdded = g.cards.some((c) => c.status === "enrolled");
    const earliest = g.cards.map((c) => c.expiryDate).filter(Boolean).sort()[0];
    const dl = daysLeft(earliest);
    const expiring = dl !== null && dl <= 7;
    const hasNotes = g.cards.some((c) => c.notes);
    return `<button class="offer" data-i="${i}">
      <div class="row1"><span class="merchant">${merchantIcon(g.merchant)}${esc(g.merchant)}</span>
        <span class="reward">${esc(g.rewardText)}</span></div>
      <div class="sub">${g.description ? esc(g.description) : ""}</div>
      <div class="cards">${g.cards.map((c) =>
        `<span class="cardchip${c.status === "enrolled" ? " added" : ""}">${esc(c.cardLabel || "default")}</span>`).join("")}</div>
      <div class="tags">
        <span class="tag ${allAdded ? "status-added" : "status-open"}">${
          allAdded ? "added ✓" : someAdded ? "added ✓ on " + g.cards.filter((c) => c.status === "enrolled").length + " of " + g.cards.length : "not added"}</span>
        ${g.channel ? `<span class="tag">${esc(g.channel)}</span>` : ""}
        ${expiring && dl >= 0 ? `<span class="tag expiring">${dl}d left</span>` : ""}
        ${hasNotes ? `<span class="tag has-notes">fine print ›</span>` : ""}
      </div></button>`;
  }).join("");
  document.querySelectorAll(".offer").forEach((el) =>
    el.addEventListener("click", () => openSheet(groups[+el.dataset.i])));
}

function openSheet(g) {
  const perCard = g.cards.map((c) => {
    const dl = daysLeft(c.expiryDate);
    return `<div class="cardblock">
      <div class="cardblock-head"><span class="cardchip${c.status === "enrolled" ? " added" : ""}">${esc(c.cardLabel || "default")}</span>
        <span class="cardblock-status ${c.status === "enrolled" ? "ok" : ""}">${c.status === "enrolled" ? "added ✓" : "not added"}</span>
        ${c.expiryDate ? `<span class="cardblock-exp">${esc(c.expiryDate)}${dl !== null ? ` (${dl < 0 ? "expired" : dl + "d"})` : ""}</span>` : ""}</div>
      ${c.notes ? `<div class="notes">${esc(c.notes)}</div>` : ""}
    </div>`;
  }).join("");
  $("#sheetBody").innerHTML = `
    <h2>${esc(g.merchant)} <span style="color:var(--accent-hi)">${esc(g.rewardText)}</span></h2>
    <div class="sub">${esc(g.bank)}${g.channel ? " · " + esc(g.channel) : ""}</div>
    ${g.description ? `<h3>Description</h3><div class="notes">${esc(g.description)}</div>` : ""}
    <h3>On your cards (${g.cards.length})</h3>${perCard}
    ${g.cards.map((c) => c.termsUrl).filter(Boolean).map(safeURL).filter(Boolean).length ? `<h3>Terms</h3><a href="${g.cards.map((c) => c.termsUrl).filter(Boolean).map(safeURL).filter(Boolean)[0]}" target="_blank" rel="noopener">offer terms ↗</a>` : ""}
    ${g.cards.map((c) => c.detailUrl).filter(Boolean).map(safeURL).filter(Boolean).length ? `<h3>At the bank</h3><a href="${g.cards.map((c) => c.detailUrl).filter(Boolean).map(safeURL).filter(Boolean)[0]}" target="_blank" rel="noopener">open the issuer's page ↗</a>` : ""}`;
  $("#sheet").classList.remove("hidden");
}

async function renderUsers() {
  let users = [];
  try { users = await fetch("/api/users").then((r) => r.json()); } catch (e) { return; }
  if (!users.length) users = ["default"];
  if (!users.includes(state.user)) state.user = users[0];
  const sel = $("#userSel");
  sel.innerHTML = users.map((u) =>
    `<option ${u === state.user ? "selected" : ""}>${esc(u)}</option>`).join("");
  sel.onchange = () => {
    state.user = sel.value;
    localStorage.setItem("oh-user", state.user);
    renderAll();
  };
}

async function renderBanks() {
  let banks = [];
  try { banks = await api("/api/banks"); } catch (e) { return; }
  $("#banks").innerHTML = banks.map((b) =>
    `<span class="bank"><span class="dot ${esc(b.status)}"></span>${esc(b.name)}:
     ${b.lastSync ? esc(shortAgo(b.lastSync)) : "never"}</span>`).join("");
  const chips = ["<button class='chip on' data-bank=''>All banks</button>"]
    .concat(banks.map((b) =>
      `<button class="chip" data-bank="${esc(b.key)}">${esc(b.name)}</button>`));
  $("#bankChips").innerHTML = chips.join("");
  document.querySelectorAll("[data-bank]").forEach((el) => el.addEventListener("click", () => {
    state.bank = el.dataset.bank;
    document.querySelectorAll("[data-bank]").forEach((x) => x.classList.toggle("on", x === el));
    renderResults();
  }));
}

function shortAgo(iso) {
  const h = (Date.now() - new Date(iso)) / 3600000;
  if (h < 1) return "<1h ago";
  if (h < 48) return `${Math.round(h)}h ago`;
  return `${Math.round(h / 24)}d ago`;
}

async function pollHuman() {
  try {
    const reqs = await api("/api/human");
    if (reqs.length) {
      const r = reqs[0];
      $("#needsyou").classList.remove("hidden");
      $("#needsyou").innerHTML = `🖐️ <b>${esc(r.bank)}</b> needs you: ${esc(r.prompt)}
        <button class="chip" id="openHuman">answer</button>`;
      $("#openHuman").onclick = () => showHuman(r);
    } else {
      $("#needsyou").classList.add("hidden");
      $("#humanSheet").classList.add("hidden");
    }
  } catch (e) { /* transient */ }
}

function showHuman(r) {
  $("#humanPrompt").textContent = r.prompt;
  $("#humanAnswer").value = "";
  $("#humanSheet").classList.remove("hidden");
  $("#humanSend").onclick = async () => {
    const answer = $("#humanAnswer").value.trim();
    if (!answer) return;
    try {
      const res = await fetch(`/api/human/${r.id}/answer`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ answer, user: r.user }),
      });
      if (!res.ok) throw new Error(String(res.status));
      $("#humanSheet").classList.add("hidden");
      pollHuman();
    } catch (e) { alert("already answered? " + e.message); }
  };
}

function renderAll() {
  renderResults();
  renderBanks();
  pollHuman();
}

$("#q").addEventListener("input", (e) => {
  clearTimeout(state.timer);
  state.q = e.target.value;
  state.timer = setTimeout(renderResults, 150);
});
document.querySelectorAll("[data-channel]").forEach((el) => el.addEventListener("click", () => {
  state.channel = el.dataset.channel;
  document.querySelectorAll("[data-channel]").forEach((x) => x.classList.toggle("on", x === el));
  renderResults();
}));
document.querySelectorAll("[data-status]").forEach((el) => el.addEventListener("click", () => {
  state.status = el.dataset.status;
  document.querySelectorAll("[data-status]").forEach((x) => x.classList.toggle("on", x === el));
  renderResults();
}));
$("#sheetClose").onclick = () => $("#sheet").classList.add("hidden");
$("#sheet").onclick = (e) => { if (e.target.id === "sheet") $("#sheet").classList.add("hidden"); };
$("#humanSheet").onclick = (e) => { if (e.target.id === "humanSheet") $("#humanSheet").classList.add("hidden"); };

renderUsers().then(renderAll);
setInterval(pollHuman, 5000);
setInterval(renderBanks, 30000);
