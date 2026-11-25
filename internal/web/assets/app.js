/* offerhound UI — no frameworks, phone-first. The server is a dumb store:
   this page only reads. A wallet dropdown switches between users. */
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

function daysLeft(iso) {
  if (!iso) return null;
  return Math.floor((new Date(iso + "T23:59:59Z") - Date.now()) / 86400000);
}

async function renderResults() {
  let offers = [];
  try {
    offers = await api("/api/search", {
      q: state.q, channel: state.channel, status: state.status, bank: state.bank,
    });
  } catch (e) { console.error(e); }
  $("#meta").textContent = offers.length
    ? `${offers.length} offer${offers.length === 1 ? "" : "s"} — tap any for the fine print`
    : "";
  if (!offers.length) {
    $("#results").innerHTML = `<div class="empty"><img class="empty-mascot" src="/assets/mascot.png" alt="offerhound">
      No matching offers in this wallet.<br>
      Try a looser term, another wallet, or run a sync from your client.</div>`;
    return;
  }
  $("#results").innerHTML = offers.map((o, i) => {
    const dl = daysLeft(o.expiryDate);
    const expiring = dl !== null && dl <= 7;
    return `<button class="offer" data-i="${i}">
      <div class="row1"><span class="merchant">${esc(o.merchant)}</span>
        <span class="reward">${esc(o.rewardText)}</span></div>
      <div class="sub">${esc(o.cardLabel)}${o.description ? " · " + esc(o.description) : ""}</div>
      <div class="tags">
        <span class="tag">${esc(o.bank)}</span>
        ${o.channel ? `<span class="tag">${esc(o.channel)}</span>` : ""}
        <span class="tag ${o.status === "enrolled" ? "enrolled" : ""}">${
          o.status === "enrolled" ? "added ✓" : "not added"}</span>
        ${expiring && dl >= 0 ? `<span class="tag expiring">${dl}d left</span>` : ""}
        ${o.notes ? `<span class="tag has-notes">fine print ›</span>` : ""}
      </div></button>`;
  }).join("");
  document.querySelectorAll(".offer").forEach((el) =>
    el.addEventListener("click", () => openSheet(offers[+el.dataset.i])));
}

function openSheet(o) {
  const dl = daysLeft(o.expiryDate);
  $("#sheetBody").innerHTML = `
    <h2>${esc(o.merchant)} <span style="color:var(--green)">${esc(o.rewardText)}</span></h2>
    <div class="sub" style="color:var(--dim)">${esc(o.cardLabel)} · ${esc(o.bank)}
      ${o.status === "enrolled" ? " · added ✓" : " · not added"}
      ${o.expiryDate ? ` · expires ${esc(o.expiryDate)}${dl !== null ? ` (${dl < 0 ? "expired" : dl + "d"})` : ""}` : ""}</div>
    ${o.description ? `<h3>Description</h3><div class="notes">${esc(o.description)}</div>` : ""}
    ${o.notes ? `<h3>The fine print ⚠️</h3><div class="notes">${esc(o.notes)}</div>` : ""}
    ${o.termsUrl ? `<h3>Terms</h3><a href="${esc(o.termsUrl)}" target="_blank" rel="noopener">${esc(o.termsUrl)}</a>` : ""}
    ${o.detailUrl ? `<h3>At the bank</h3><a href="${esc(o.detailUrl)}" target="_blank" rel="noopener">open the issuer's page ↗</a>` : ""}`;
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
        body: JSON.stringify({ answer }),
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

renderUsers().then(renderAll);
setInterval(pollHuman, 5000);
setInterval(renderBanks, 30000);
