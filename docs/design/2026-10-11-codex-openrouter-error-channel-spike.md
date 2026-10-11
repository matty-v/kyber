# Codex OpenRouter error and channel evidence (MAT-116)

**Status:** partial spike. Synthetic Codex 0.162.1 error evidence is below. No live OpenRouter provider 403 or disposable channel round trip has been run; those remain required before qualification.

## Method

Ran `codex exec` 0.162.1 with an isolated temporary `CODEX_HOME`, synthetic credential, and a local Responses API server. Each case sent a simple prompt and returned the specified HTTP status with a synthetic error body. Only the final `task_complete.error.codex_error_info` was inspected. The temporary home was removed after each run. The test did not create a Kyber Agent or use production credentials.

Kyber's current reporter (`cmd/codex-reporter/main.go`) reads a post-start `task_complete` from the current rollout and sends `inference_auth_rejected` only when `pkg/tokenreport/codex.go` finds `codex_error_info.http_connection_failed.http_status_code == 401`. The API accepts that signal only from the current running Codex endpoint pod UID; the controller then moves it to `NeedsAuth`. Other errors are ordinary failed turns under the current code path. This is a source-based phase expectation, not an observed dev Agent phase.

| Synthetic case | Codex 0.162.1 final structured error | Expected Kyber phase from current code | Live Kyber dev phase |
| --- | --- | --- | --- |
| 401 credential rejection | `http_connection_failed.http_status_code=401` | `NeedsAuth` | Not yet tested |
| 403 provider/model rejection | `http_connection_failed.http_status_code=403` | Remains `Running`; failed turn visible in rollout | Not yet tested |
| 429 rate limit | `response_too_many_failed_attempts.http_status_code=429` after retries | Remains `Running` | Not yet tested |
| 500 service error | `internal_server_error` after retries | Remains `Running` | Not yet tested |
| Connection refused | No final `task_complete` within a 50-second bounded probe | No 401 signal was observed; final phase/recovery unknown | Not yet tested |
| Timeout, DNS, TLS, transient recovery | Not yet probed | Must not strand the Agent in `NeedsAuth` | Not yet tested |

The synthetic 403 establishes the parser's distinction from 401 for this Codex version. It does not establish that OpenRouter returns the same error envelope for every upstream provider denial, or that a running Kyber Agent surfaces a useful operator message. A log string containing `unauthorized` is deliberately insufficient to enter `NeedsAuth`.

## Remaining qualification

1. In Kyber dev, use a disposable Codex Agent and a controlled cluster-internal Responses fixture to induce 401, 403, 429, 5xx, timeout, and transport errors. Record Agent phase, task error, operator message, and recovery after the fixture returns success; rotate the Secret only for true credential rejection. Remove the Agent, Secret, fixture, Service, and PVC afterward.
2. Use a disposable channel bot/recipient to prove inbound delivery, Codex turn, outbound reply, and controlled pod replacement on `openai/gpt-4.1-mini`. No production channel token should be reused. Matt has been asked for a disposable Telegram bot token and to start a chat with it.
3. Record the exact Codex image/version and OpenRouter route/model used, and separate configured route from actual upstream provider attribution. Add focused regression tests for any observed phase or channel defect. Keep MAT-108's remaining qualification claims open until these tests and MAT-115 are complete.
