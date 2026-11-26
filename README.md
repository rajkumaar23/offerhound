# offerhound 🐕💳

<p align="center">
  <img src="docs/assets/offerhound-mascot.png" width="200" alt="offerhound mascot: a hound in a monocle holding a fan of credit cards">
</p>

**Your cards sniff out their own offers.** Self-hosted credit-card offer
aggregator for portals that deliberately hide offers behind your login
(major issuer offer portals, …). A declarative per-bank
YAML DSL drives a real browser over CDP; a mobile-first web UI answers the
only question that matters at the register or checkout:

> "Does **any** of my cards have an offer at **this** merchant, right now?"

```
offerhound sync examplebank      # log in (headless or visible Chrome), collect
offerhound search "target"     # best card across your whole wallet
offerhound serve               # phone-friendly web UI on your LAN
offerhound doctor              # validate browser/display/sandbox before a sync
```

- **No bank code in the binary** — banks are YAML configs,
  so adding a bank is a PR to YAML, not Go.
- **Headless by default, headful when a bank insists** — per-bank `browser:`
  config; `mode: attach` connects to your *real* browser over a CDP URL
  (the Cloudflare escape hatch).
- **Human-in-the-loop 2FA done right** — SMS/push steps pause the run,
  notify you (web badge + webhook), and wait for your answer. No guessing,
  no lockouts, no timed pressure.
- **Read-only by construction** — the DSL validator rejects enroll/click
  steps in offer flows. Your cards' offer slots are sacred.
- **Fine print is first-class** — "up to 7X" almost always means *some*
  categories only. Bank sub-text lands in a `notes` field that is searchable
  and pops up as a bottom sheet on every offer card — never burn points on a
  misread rate again.
- **Local-first, multi-user by default** — SQLite store with per-wallet
  `user` scoping; credentials only from env/file/keyring refs. The server is
  deliberately dumb: clients push when they want. Reads are open (card
  offers are marketing data); writes need `X-Offerhound-Token`, checked
  against a SHA-256 hash — `serve --token-hash` keeps the plaintext off the
  server entirely.

## Quick start

```sh
go install github.com/rajkumaar23/offerhound/cmd/offerhound@latest
offerhound init
export OH_EXAMPLEBANK_USER=... OH_EXAMPLEBANK_PASS=...
offerhound sync examplebank
offerhound search delta
offerhound serve --listen 0.0.0.0:8090   # token printed once
offerhound serve --no-remote-icons       # keep browser pings off the icon CDN
```

Bring your own bank configs: drop YAML files into `~/.offerhound/configs`
(start from [docs/examples](docs/examples/)).

Requires Google Chrome / Chromium on the host (or a CDP-reachable browser for
`mode: attach`).

## Docker

```sh
docker run -d --name offerhound -p 8090:8090 \
  -v ~/.offerhound:/data \
  ghcr.io/rajkumaar23/offerhound
```

The image serves the UI by default and *also* carries Chromium, so one-off
syncs from cron work too:

```sh
docker run --rm -v ~/.offerhound:/data \
  -e OH_EXAMPLEBANK_USER=... -e OH_EXAMPLEBANK_PASS=... \
  ghcr.io/rajkumaar23/offerhound sync examplebank --configs /data/configs
```

**Headful automation in a container is guarded, not guessed.** `offerhound
doctor` (wired in as the container `HEALTHCHECK`) validates the environment
before anything bank-facing runs:

| Capability | In the container | Notes |
| --- | --- | --- |
| Headless Chromium | ✅ works | ships `--no-sandbox,--disable-dev-shm-usage` (override via `OFFERHOUND_BROWSER_FLAGS`) |
| Headful Chromium | ⚠️ needs X | run under `xvfb-run -a` (Xvfb is bundled), or flip the bank to `headless: true` |
| `mode: attach` | ✅ works | point `cdpUrl` at any CDP-reachable browser |
| serve / UI | ✅ default CMD | `HEALTHCHECK` probes `/api/users` + browser sanity |

A bank config asking for `headless: false` without a display fails fast with
that exact advice — never a chromium stack trace. Pin versions with
`ghcr.io/rajkumaar23/offerhound:0.1.0` (multi-arch: amd64, arm64).

## Docs

- [Design](docs/DESIGN.md) — architecture, DSL spec, 2FA flow, roadmap
- [Example bank configs](docs/examples/)

## Status

v0.1 — DSL engine, store, CLI and mobile web UI are working and covered by
tests (`go test ./...`). Everything under `docs/examples/` is **synthetic**:
working per-bank configs are personal artifacts and stay private; community
configs go through sanitization review before merging (see [CONTRIBUTING](CONTRIBUTING.md)).

## Why not Plaid/aggregators?

Card *offers* are targeted marketing surfaces: unique per user, login-only,
and not exposed by any public API (verified per-bank — see
[DESIGN.md §1](docs/DESIGN.md)). The only honest source is your own session,
so offerhound automates exactly that, politely.

## License

MIT
