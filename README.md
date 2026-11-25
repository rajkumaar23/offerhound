# offerhound 🐕💳

**Your cards sniff out their own offers.** Self-hosted credit-card offer
aggregator for portals that deliberately hide offers behind your login
(Chase Offers, Amex Offers, Capital One Offers, …). A declarative per-bank
YAML DSL drives a real browser over CDP; a mobile-first web UI answers the
only question that matters at the register or checkout:

> "Does **any** of my cards have an offer at **this** merchant, right now?"

```
offerhound sync chase          # log in (headless or visible Chrome), collect
offerhound search "target"     # best card across your whole wallet
offerhound serve               # phone-friendly web UI on your LAN
```

- **No bank code in the binary** — banks are YAML configs
  (the `offerhound-configs` repo, coming soon),
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
- **Local-first** — SQLite store, credentials from env/keyring, UI bound to
  localhost until you opt into LAN with a token.

## Quick start

```sh
go install github.com/rajkumaar23/offerhound/cmd/offerhound@latest
offerhound init
export OH_EXAMPLEBANK_USER=... OH_EXAMPLEBANK_PASS=...
offerhound sync examplebank
offerhound search delta
offerhound serve --listen 0.0.0.0:8090   # token printed once
```

Bring your own bank configs: drop YAML files into `~/.offerhound/configs`
(start from [docs/examples](docs/examples/), or get community configs from the `offerhound-configs`
repo (coming soon)).

Requires Google Chrome / Chromium on the host (or a CDP-reachable browser for
`mode: attach`).

## Docs

- [Design](docs/DESIGN.md) — architecture, DSL spec, 2FA flow, roadmap
- [Example bank configs](docs/examples/)
- Community bank configs: the `offerhound-configs` repo (coming soon)

## Status

v0.1 — DSL engine, store, CLI and mobile web UI are working and covered by
tests (`go test ./...`). Everything under `docs/examples/` is **synthetic**:
working per-bank configs are personal artifacts and stay private; community
configs ship in the `offerhound-configs` repo
after sanitization review (see [CONTRIBUTING](CONTRIBUTING.md)).

## Why not Plaid/aggregators?

Card *offers* are targeted marketing surfaces: unique per user, login-only,
and not exposed by any public API (verified per-bank — see
[DESIGN.md §1](docs/DESIGN.md)). The only honest source is your own session,
so offerhound automates exactly that, politely.

## License

MIT
