---
description: Go conventions for workbox
globs: ["**/*.go", "go.mod", "go.sum"]
---

# Go rules

- Write idiomatic, boring Go. Prefer clarity over cleverness.
- **External services go behind interfaces.** Cloud calls (Compute, Firestore)
  are reached through the `compute.Compute` and `state.Store` interfaces, each
  with a fake in the same package. New cloud dependencies follow the same shape
  so command and schedule logic stay unit-testable without real GCP.
- **Never shell out for an API that has an official Go client.** Use the Google
  Cloud Go libraries for Compute and Firestore. Shelling out is reserved for
  genuinely external tools with no library (`ssh`, `herdr`).
- Model Compute states as the explicit `compute.State` set; do not scatter raw
  status strings through the code.
- **Context:** every operation takes `ctx` and honors cancellation. Long waits
  select on `ctx.Done()`. Ctrl-C is wired via `signal.NotifyContext`.
- **Clocks are injected.** Schedule-dependent code takes a `Now func() time.Time`
  (or equivalent); tests never depend on the wall clock.
- **Errors** are wrapped with `%w` and context (`fmt.Errorf("resuming instance: %w", err)`),
  concise and actionable. Sentinel errors (`ErrUnsupportedState`, `ErrChecksFailed`)
  are compared with `errors.Is`.
- Keep `gofmt` clean and `golangci-lint` (govet, staticcheck, errcheck,
  ineffassign, unused) quiet. `make lint` runs it; `make check` includes it.
- Tests are first-class: schedule math, config precedence/parsing, path
  expansion, idempotent wake/sleep, status JSON, cancellation. Table-driven
  where it helps. Do not write flaky time-based tests.
