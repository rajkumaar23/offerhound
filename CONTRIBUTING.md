# Contributing

## Bank configs: the one hard rule

**Never publish a real, working bank config.** Working configs encode
account-specific knowledge (endpoints, selectors, rate budgets) and must
stay on your own machines. Everything in this repository's
`docs/examples/` is synthetic and points at `*.example.test`.

When contributing configs (to
[offerhound-configs](https://github.com/rajkumaar23/offerhound-configs)):

1. Remove credentials — `env://`, `file://`, `keyring://` references only.
2. Remove anything account-specific (IDs, hashes, personal URLs).
3. Keep prompts generic ("the code your bank texted you", not real
   screenshots or message text).
4. Configs are reviewed and sanitized by maintainers before merge.

## Code

- `make all` runs gofmt check, vet, race tests, build.
- Tests are table-driven; new step types need loader validation + a test.
- Keep the read-only rule absolute: no click/enroll steps in offer flows.

## Be polite to issuers

Configs must set `rateLimit:` budgets. Automation that hammers issuer
endpoints gets everyone blocked.
