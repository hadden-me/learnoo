# Local renderer experiment

This is the LRN-004 renderer experiment, not the production application. It deliberately has no accounts, database, teacher workflow or publishing. It binds only to 127.0.0.1 and accepts only fixtures marked `unreviewed`. Do not expose it through a public reverse proxy or tunnel.

## Prerequisites

- Go 1.25 or newer (tested with 1.25.4).
- Node.js 22 and npm (tested with Node 22.20.0).

## Run

From the repository root:

```sh
npm ci --ignore-scripts
npm run assets
go run ./cmd/renderer
```

Open http://127.0.0.1:8080/lesson. Change the local port with `go run ./cmd/renderer -port 8081`.

- `/`: landing-page UI example; local only, no registration or publication.
- `/lesson`: unreviewed French complex-numbers fixture.
- `/bidi`: isolated Arabic/French directionality fixture.
- `/failure`: intentionally invalid formula with readable fallback.
- `/lab`: edit raw JSON, preview with the same Go renderer, save/reopen on the current browser only.
- `POST /preview`: validates JSON and returns the canonical HTML used by the reader.

Browser-side KaTeX is copied from the pinned npm package and served locally with its license. No CDN requests are needed. YouTube loads only on a click when an actual video ID is present; the initial lesson has a pending-selection placeholder. The original logo banner is not loaded on every lesson page.

The JSON laboratory is not TipTap. No server-side draft storage or approval behavior is simulated. See [fixture details](fixtures/README.md).

## Design foundations

The reader consumes generated design tokens. Edit `web/design/tokens.json`, run `npm run design:build`, and rebuild the Go server. See [shared design exports](../web/design/README.md) for plain CSS and future Tailwind v4 Studio integration.

## Verify

```sh
npm run design:check
npm run design:contrast
go test ./...
go vet ./...
npx playwright install chromium
npm run test:browser
go test ./internal/rendering -run '^$' -bench BenchmarkRenderLesson -benchmem
```

Playwright always starts its own loopback server on port 18080 and refuses to reuse an existing process, in development and CI alike. Set `LEARNOO_TEST_PORT=18081` to select another port. Tests derive URLs and request-origin checks from this configuration; the interactive preview on 8080 is independent. Results and screenshots are written to ignored `test-results/`. Run `npm run assets` before compiling: Go embeds the prepared local browser assets into the binary.

For a restricted environment, put the Go build cache in a writable temporary directory by prefixing commands with `GOCACHE=/tmp/learnoo-go-cache`.

## Continuous integration

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) runs the commands above on every pull request to `develop` or `main` (including fork PRs) and on pushes to those branches. GitHub reports the check as **`verify`** under the **CI** workflow; select that check from the list when configuring branch protection rather than typing a name. It runs on Ubuntu 24.04 with Go 1.25.x and Node 22, in this order: `npm ci --ignore-scripts`, `npm run design:check`, `npm run assets`, `npm run design:contrast`, `go test ./...`, `go vet ./...`, `npx playwright install --with-deps chromium`, `npm run test:browser`. Any failing step fails the job; no step is allowed to fail.

Security: the workflow uses the `pull_request` trigger (never `pull_request_target`), a read-only `contents: read` token, no repository secrets, and checkout without persisted credentials. Third-party actions are pinned to commit SHAs. If a step fails, the `test-results/` folder (local fixture screenshots and metrics only) is uploaded as the `playwright-test-results` artifact for 7 days.

`design:check` runs before `assets` on purpose: `assets` regenerates the design files, so checking afterwards would hide generated CSS that was not committed. Making `verify` a required status check in branch protection is a maintainer setting and is not changed by the workflow.

## Security limits

Schema allowlists, escaped templates, local image allowlists, content/depth/formula limits, CSP, same-origin preview checks, and KaTeX `trust: false` reduce rendering risks. They do not establish production security. The only HTTP mutation is a read-only rendering request; browser saves are local fixtures. Authentication, durable storage, role checks, review and publication invariants belong to later work and remain required for the real platform.

## Dependency licensing

KaTeX is MIT-licensed and Playwright is Apache-2.0-licensed; their own terms apply. The project's interim notice does not override dependencies. `scripts/copy-katex.mjs` preserves the KaTeX license in the generated vendor directory.
