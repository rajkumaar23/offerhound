# offerhound — design

> Your cards sniff out their own offers. Open-source, self-hosted, declarative.

## 1. What it is

`offerhound` is a single Go binary that:

1. **Logs into your bank/issuer portals** using a declarative, per-bank YAML
   config (the **DSL**) — no per-bank Go code.
2. **Extracts your personalized, login-only card offers** (Amex Offers,
   Chase Offers, Capital One Offers, …) into a local SQLite database with a
   normalized schema.
3. **Serves a mobile-first web UI** for the two real moments of use:
   - *at the shop counter*: "does **any** of my cards have an **in-store**
     offer for this merchant, right now?"
   - *about to buy online*: "which card gives the most back for **this**
     vendor?"

It is deliberately **not** an offer search engine: personalized offers only
exist inside your logged-in session (verified across issuers — no public API,
aggregators like Plaid do not expose them). So offerhound is a *browser
automation* tool with a data pipeline, not a crawler.

### Non-goals
- Never enrolls/adds offers (banks impose per-user and per-offer enrollment
  caps — scraping is read-only by design; `click` steps are forbidden in
  `offers` flows by config validation).
- Never stores plaintext credentials — they come from env vars or the OS
  keyring at run time.
- No cloud service. Everything is local (or LAN). Telemetry: none.

## 2. Architecture

### Deployment topology — UI and scraper are independent (this is the point)

The browser lives where *a real browser* lives (the user's Mac, a desktop
server, a VM with Chrome); the phone needs the UI from *somewhere always up*
(homelab server, VPS). So offerhound splits cleanly:

```
   Mac / any browser host                    always-on host (VPS / homelab)
 ┌──────────────────────┐                 ┌─────────────────────────────�──┐
 │ offerhound worker    │   HTTPS,        │ offerhound serve              │
 │  - claims jobs       │◀──outbound──▶   │  - SQLite (source of truth)   │
 │  - runs chromedp     │   only          │  - web UI (phone)             │
 │    (launch | attach  │                 │  - /api/ingest  (bulk upsert) │
 │     to Mac's Brave)  │                 │  - /api/jobs    (queue)       │
 │  - posts results +   │                 │  - human-request relay        │
 │    needs_you + shots │                 │  - auth token                 │
 └──────────────────────┘                 └───────────────────────────────┘
        ▲    `offerhound sync --server URL` also works one-shot (cron on the
        │     browser host pushing to the server); `worker` is the long-lived
        ▼     poll-claim-report loop that survives NAT (no inbound ports).
   [real Chrome/Brave, user sessions, profile dirs]
```

- **Colocated mode** (single machine): `offerhound sync` writes the local
  SQLite directly; `serve` reads it. Zero network.
- **Split mode** (normal for phone access): `serve` on the always-on host is
  the only writer of truth; browser hosts run `offerhound worker
  --server https://… --token …` (or cron'd `sync --server …`). All traffic
  is **outbound from the browser host** — no port-forwarding onto the Mac.
- One server can be fed by N worker hosts (Mac for Cap One's attach-mode,
  a VM for headless Chase). Workers register `(hostID, banks[])`; the UI's
  "Run now" enqueues a job any eligible worker claims.
- **Human 2FA crosses the seam transparently**: worker POSTs
  `needs_you{prompt, screenshot}` → server surfaces it in the phone UI →
  answer travels back inside the worker's next job poll (outbound-only).

### Component sketch (colocated view)

```
                    ┌─────────────────────────────────────────┐
                    │              offerhound (1 binary)      │
                    │                                         │
 CLI (cobra)        │  ┌──────────┐   ┌────────────────────┐  │
 offerhound sync ───┼─▶│ Scheduler│──▶│   Engine (per bank)│  │
 offerhound serve ──┼─▶│ robfig/  │   │  chromedp driver   │  │
 offerhound search  │  │ cron     │   │  launch | attach   │  │
                    │  └──────────┘   │  step interpreter  │  │
                    │                 │  human gate (2FA)  │  │
   Browser          │                 └─────────┬──────────┘  │
 ┌───────────┐      │               normalized  │ offers      │
 │ real      │◀─────┼── attach (CDP over LAN)   ▼             │
 │ Chrome /  │      │                 ┌────────────────────┐  │
 │ Brave     │      │                 │ store (SQLite:     │  │
 └───────────┘      │                 │ offers, banks,     │  │
   (user's machine) │                 │ runs, human_reqs)  │  │
                    │                 └─────────┬──────────┘  │
                    │  ┌────────────────────┐   │             │
                    │  │ web (htmx + stdlib)│◀──┘             │
                    │  │ mobile-first UI    │                 │
                    │  └────────────────────┘                 │
                    └─────────────────────────────────────────┘
```

- **Engine** drives Chrome over CDP via [chromedp](https://github.com/chromedp/chromedp).
- **Two browser modes**, per bank, configurable:
  - `launch`: offerhound starts its own Chrome (default). `headless: true` by
    default; set `headless: false` for banks that fingerprint headless.
  - `attach`: connect to an already-running browser over a CDP URL —
    e.g. a browser you keep signed in on your desktop, reached over a LAN CDP
    URL. This is the escape hatch for CDN-challenged portals that block
    datacenter-style automation but walk right into a real user's browser,
    and for banks whose sessions live in the user's everyday browser.
- **Sessions persist** per bank in `profileDir` (cookies survive across
  runs). Every sync starts with a cheap `probe`; the `login` flow only runs
  when the probe says we're signed out.
- **Rate limiting is a first-class config concern** (`rateLimit:`): issuers
  429 aggressively (measured budgets of ~20–25 grid requests/hour on
  mainstream issuers). The
  engine enforces `requestsPerRun` + `delayBetween` + honors `Retry-After`.
- **Partial runs never clobber good data**: a bank's offers are replaced
  atomically only if the run completed every declared source; failed sources
  keep last-known data and mark the bank `stale`/`partial`.

## 3. The DSL

One YAML file per bank (lives in the separate **offerhound-configs** repo;
`--configs` dir or embedded defaults). Shape: tagged polymorphic steps with
up-front validation, so configs fail loudly in CI instead of silently in
production.

```yaml
# banks/examplebank.yaml — fully synthetic illustration of the DSL
id: examplebank
name: Example Bank Offers
baseUrl: https://offers.example.test

browser:
  mode: launch            # launch | attach
  headless: true          # false ⇒ real window (banks that fingerprint)
  profileDir: ~/.offerhound/profiles/examplebank
  # mode: attach          # connect to an already-running real browser
  # cdpUrl: http://desktop.local:9222

rateLimit:
  delayBetween: 1200ms
  requestsPerRun: 30

credentials:
  username: env://OH_EXAMPLEBANK_USER     # or keyring://offerhound/examplebank
  password: env://OH_EXAMPLEBANK_PASS     # literals are rejected by validation

login:
  probe:                                   # cheap signed-in check
    fetchApi: /api/whoami
    okStatus: [200]
  steps:                                   # only run when probe fails
    - navigate: https://offers.example.test/login
    - waitSelector: { selector: "input[name=username]", timeout: 15s }
    - fill: { selector: "input[name=username]", valueFrom: credential.username }
    - fill: { selector: "input[name=password]", valueFrom: credential.password }
    - click:  { selector: "button[type=submit]", trusted: true }
    - human:                               # SMS/push/OTP — pauses, never fails silently
        when: { selector: "input[name=otp]", timeout: 8s }   # only fires if it appears
        prompt: "Enter the 6-digit code Example Bank texted you"
        fill:   { selector: "input[name=otp]", submit: "button.verify" }
        timeout: 10m                       # then run fails as 'needs_you'

offers:
  - name: accounts
    fetchApi: /api/accounts
    saveAs: accounts                       # keep raw JSON for `each:` loops
    cards:                                 # card metadata for the UI chips
      each: "$[*]"
      cardLabel: "{{.productName}} …{{.last4}}"

  - name: grid                             # the eligible-to-add offers
    each: { from: accounts, items: "$[*]", as: acct }
    fetchApi: "/api/accounts/{{urlquery .acct.id}}/offers?pageSize=50"
    paginate: { cursorParam: cursor, cursorPath: nextCursor, maxPages: 10 }
    extract:
      items: "$.data[*]"
      fields:
        merchant:   merchantName
        rewardText: headline
        channel:    terms.channel          # "Online" | "In-Store" | …
        notes:      terms.finePrint        # category rates, exclusions — shown in the UI sheet
      status: eligible

  - name: activated                        # offers already added to the card
    each: { from: accounts, items: "$[*]", as: acct }
    fetchApi: "/api/accounts/{{urlquery .acct.id}}/activated"
    devalue: true                          # decode SvelteKit flat-array wire format
    extract:
      items: "activatedOffers[*]"
      fields:
        merchant:   merchantName
        rewardText: headline
        channel:    terms.channel
        notes:      terms.finePrint
        expiryDate: { path: badge.text, transform: daysLeftToDate }  # "7 days left" → 2026-12-02
      status: enrolled                     # enrolled wins over eligible on dedupe
```

### Step taxonomy (v1)

| step | purpose | notable options |
|---|---|---|
| `navigate` | goto URL | |
| `waitSelector` / `waitNotVisible` | sync on SPA | `timeout` mandatory — never unbounded |
| `click` | click element | `trusted: true` ⇒ CDP `Input.dispatchMouseEvent` (React popups block synthetic clicks); `js_path` for generated-Id DOM |
| `fill` | type into input | `value` / `valueFrom: credential.X`; native-setter + input events for React |
| `fetchApi` | **in-tab `fetch()` with cookies** | template URL, `saveAs`, `devalue: true`, `headers` |
| `paginate` | follow cursors | `cursorParam`, `cursorPath`, `maxPages`, loop-back stop |
| `each` | loop over JSON array | `from: <saveAs>`, `items: <jsonpath>`, `as:` |
| `extract` | map JSON ⇒ offers | `items` path; `fields` (dotted path, `first`, `transform`); `status` const |
| `human` | 2FA / captcha gate | `when` (conditional), `prompt`, `fill{selector,submit}`, `timeout` |
| `screenshot` | evidence | attached to run record; shown in UI on failure |
| `sleep` / `reload` | pacing | |

`click`/`fill` are **rejected by validation inside `offers:`** (read-only rule).

### Why `fetchApi` instead of scraping?
Modern bank frontends (often SvelteKit/Next SPAs) already fetch clean
JSON; replaying those calls from inside the logged-in tab gives stable
structured data while keeping the browser's cookies, origin, and TLS/HTTP2
fingerprint. DOM `extract` (selector-based) remains a v1.x addition for
server-rendered banks; the DSL keeps one `extract` vocabulary either way.

## 4. Human-in-the-loop (SMS 2FA & friends)

Some issuers force SMS/push regardless of browser realism. Design
principle: **human-paced steps impose no timed pressure; the machine
waits, never guesses.**

1. `human` step fires → run transitions to `needs_you`, screenshot saved.
2. Notifiers fire: web UI pending badge (htmx poll), optional generic
   webhook `POST {bank, prompt, runID, screenshotURL}` (user pipes to
   Signal/Telegram/ntfy themselves — v1 ships webhook, no platform lock-in).
3. Web UI shows the prompt + live screenshot + input box. Submit resolves the
   blocked step (value delivered over an authenticated local token).
4. Timeout ⇒ run marked `needs_you (expired)`, last-good data preserved,
   next run re-attempts from `probe` (session may have been completed by the
   user in the meantime).

`human.always: true` supports "captcha every login" banks: machine fills
credentials, **human** solves + submits — a proven pattern for
captcha-gated issuers.

### Notes & fine print (the "up to 7X" problem)

"Up to 7X miles" almost always means *only certain categories* earn 7X;
the rest earn less. Issuers expose this as sub-text, category tables, or
terms links — the DSL maps any of them into `Offer.Notes`:

```yaml
    extract:
      items: "$.data[*]"
      fields:
        rewardText: buttonText
        notes:      { path: subText }                    # plain fine print
        # or compose: notes: { path: categories, transform: join }
        termsUrl:   { path: termsLink }
```

The web UI renders a tap-to-open **detail sheet** per offer: full notes,
per-category rates when present, terms link, raw JSON for debugging
(power-users), and a "why is this ambiguous" hint linking to the issuer's
own offer page. Notes are scraped eagerly at collect time — the counter is
not the place to discover exclusions.

## 5. Data model (SQLite, modernc.org/sqlite — no cgo)

```
banks(key PK, name, status(ok|stale|partial|needs_you|error|never),
      last_sync, note, cards_json)
offers(id PK, bank FK, card_label, merchant, title, description,
       reward_text, reward_value REAL, channel, status(eligible|enrolled),
       notes, terms_url, expiry_date, source, detail_url, scraped_at,
       UNIQUE(bank, card_label, merchant, reward_text))
runs(id PK, bank, started, finished, status, error, screenshot_path)
human_requests(id PK, run_id, prompt, screenshot_path, state(pending|answered|expired),
               created, responded)
```
Search: SQLite FTS5 over merchant/title/description + `LIKE` fallback;
channel filter powers the in-store mode. Writes go through the collector-API
pattern (token-authed local POST /api/ingest) so external collectors
(e.g. the CDP-relay script) stay compatible.

## 6. Web UI (mobile-first is the *point*)

- Stack: Go `net/http` + templates + **htmx** + vanilla CSS (no node, no
  build step — single binary stays true). Dark theme, thumb-reachable.
- Home = **one giant search box** (auto-focus) + two big chips:
  `In-store now` / `Online`. Typing "target" instantly answers across all
  cards: merchant, reward, per-card rows, expiry countdown, added✓ badge.
  **Tapping a card opens the detail sheet** (notes, per-category rates,
  terms link — the "up to 7X" fine print).
- Bank status strip (green/amber/red + last-sync + "Run now" per bank).
- `Needs you` card with screenshot + OTP form (the 2FA flow above).
- PWA manifest + offline shell (stale data still useful at the register where
  reception dies; freshness shown honestly, never hidden).

## 7. Security posture

- Credentials only via `env://` / OS keyring (`keyring://`) at run time;
  never in YAML, logs, DB, or the UI. Config lint (`offerhound verify`) flags
  suspicious literal-looking credential fields.
- Web UI binds `127.0.0.1` by default; LAN exposure requires explicit
  `--listen` **and** generates an access token printed once.
- TLS interception: none. attach-mode CDP URLs are LAN-trust.
- All automation is read-only by construction (see validation rule above).
- Users run this with their own accounts under their own ToS — README says
  the quiet-part-out-loud: banks may rate-limit or block automation; that is
  the user's risk, offerhound only paces politely.

## 8. Repo split

- **offerhound** (this repo): engine, DSL, store, web, CLI. Go standards:
  table-driven tests, `golangci-lint` (errcheck, govet, staticcheck),
  `goreleaser` for multi-arch binaries, conventional commits, `go.mod` with
  only: `chromedp`, `modernc.org/sqlite`, `yaml.v3`, `robfig/cron/v3`,
  `spf13/cobra`, `tidwall/gjson` (jsonpath), `99designs/keyring`.
- **offerhound-configs**: `banks/*.yaml` + fixtures + schema docs. Engine
  pins compatible schema versions; `offerhound verify banks/` runs in that
  repo's CI so community configs can't merge broken.

## 9. Roadmap

| mile | deliverable | acceptance |
|---|---|---|
| **M0** (this PR) | skeleton: cobra CLI (`sync/serve/verify/search/init`), config loader + validator, step structs, store + migration, web shell with search UI, CI, goreleaser | `go build ./... && go vet && golangci-lint && go test` green; `offerhound verify docs/examples` passes |
| **M1** | chromedp engine: launch/attach, probe-login short-circuit, fetchApi/paginate/devalue/extract, rate limiter, partial-safe writes | e2e test against a local fake-bank fixture server (golden HTTP) |
| **M2** | human gate + webhooks + screenshots; keyring creds; scheduler + alerts (new/expiring offers via webhook) | manual: SMS-2FA bank completes via phone in <2 min |
| **M3** | community configs repo seeds (sanitized per CONTRIBUTING); docs site; screenshots/GIF | 3 banks green on the maintainer's cards |
| **M4** | PWA offline mode, share-to-hound (iOS share a vendor URL ⇒ instant best-card), Docker image | store-counter usability dogfood |

## 10. Name

**offerhound** — the hound sniffs every card so you don't have to log into
any of them. CLI: `offerhound sync chase`. Mascot: a bloodhound with a
monocle holding four credit cards in its mouth (design welcome as a PR 🙂).
