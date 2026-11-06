# Review guidelines

## Scope

- Report a defect only when the PR introduces it, makes it newly reachable, or changes its
  protection. Cite the changed lines and the path that reaches the failure.
- If the behavior is the same on `main`, do not report it as a bug in this PR. You may add one
  short note marked as pre-existing, at the lowest severity. Do not ask the author to fix it in
  this PR.
- Before you flag a missing check, look for it in the callers, in `livekit/protocol` validation
  (`livekit/sip.go`, `sip/`), and in `sipgo`. Validation that runs before the request reaches this
  service is valid.
- Give a concrete SIP or RTP sequence that triggers the failure: the messages, their order, and
  the result. A path that only a misbehaving peer can reach is still in scope. A path that needs
  an impossible internal state is not.

## Sensitive data in logs, errors, and telemetry

- Report a leak only when you can name the sensitive value, trace a reachable path from its
  source to a log line, error string, span, metric label, or analytics event, and explain why
  nothing redacts it. Cite the code. A field that can hold arbitrary text is not evidence of a
  leak.
- Secrets must never be emitted, at any log level.

## Severity

- 🔴: crash, deadlock, dropped or stuck call, one-way or no audio, wrong billing or call status,
  security or secret exposure.
- 🟡: wrong metrics or logs, degraded behavior in an edge case, missing test for a changed path.
- Do not use 🔴 for pre-existing behavior.
