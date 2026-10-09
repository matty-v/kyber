# Opt-in Ori Eval model screening (MAT-110)

This is a **manual external job** for screening OpenRouter models on the synthetic [MAT-109 fixture](../examples/ori-eval/README.md). It does not run a Kyber agent, test channels or durable tasks, or change an agent's model. The operator reviews the aggregate result and uses Kyber's existing model action separately if appropriate.

## Execution boundary

Run the job on a disposable CI runner or container that has no production agent PVC, identity repository, channel credentials, Kubernetes credentials, or Kyber write credential. The Python runner makes a temporary workspace, sets Ori's `HOME` and `TMPDIR` inside it, and passes only a small environment allowlist, but **a temporary directory is not an OS sandbox**. Ori's own agent can have shell/file tools; a model instruction saying “do not use tools” is not an access control. The runner rejects model calls from a Kyber agent pod. Give the external runner only an OpenRouter key and the reviewed fixture. Destroy the runner after extracting its aggregate JSON. Do not upload Ori's raw stdout, stderr, history, or temporary workspace.

The [manual GitHub workflow](../../.github/workflows/ori-eval-comparison.yml) provides that disposable external runner. It has read-only repository permission, does not persist checkout credentials, and uploads only the aggregate JSON for seven days. It is never triggered by a push or pull request. A repository administrator must configure a dedicated `ORI_EVAL_OPENROUTER_API_KEY` Actions secret before the workflow can make model calls; the key injected into a Kyber agent pod is not copied into GitHub. The workflow's `full` and `allow_uncapped` inputs make those choices explicit.

The key should be dedicated to evals with a provider-enforced spend limit. The script queries `/api/v1/key` and refuses uncapped or exhausted keys before model calls. `--allow-uncapped` is a visible one-run waiver for an operator who has accepted that risk; Matt used it for the bounded investigation. Ori's pilot and `toCostAtMost` assertions report cost after requests, so neither is a pre-call budget. Keep the model slate, turns, output-token cap, test timeout, and process timeout reviewed in source.

## Run

Install the exact Linux x64 Ori binary `0.15.6+4855f79` (SHA-256 `d3525283d0431197943445c499edf8d790d13816752efd0e373afe2da75e035c`) and Bun `1.4.2` on the disposable runner. The script verifies both. Supply `OPENROUTER_API_KEY` from that runner's secret store; do not paste it into commands, files, reports, or PRs.

```sh
python3 scripts/ori-eval-comparison.py --ori /path/to/ori --bun /path/to/bun --preflight-only
python3 scripts/ori-eval-comparison.py --ori /path/to/ori --bun /path/to/bun --full --output aggregate.json
```

The first command discovers and imports four cases without a key or model call. The second runs a one-case-per-model pilot and checks OpenRouter generation metadata, then runs the full four-case comparison if the pilot tests pass without a hard model/cost mismatch. Omit `--full` to stop after the pilot. For a deliberately uncapped key, add `--allow-uncapped` to the second command after authorization. A failed pilot or unexpected extra model stops further calls. OpenRouter generation lookup has returned intermittent 404s for real runs; the job can finish its bounded sweep under the key limit or explicit waiver, but exits nonzero and reports incomplete cost provenance if records are still unavailable.

The aggregate JSON contains fixture hash, pinned versions, selected model names, pass/fail, mean duration, Ori-reported cost, verified candidate cost, verified ancillary Decisions API cost, candidate versus ancillary provider names, and any provenance problems or warnings. It omits generation IDs, raw prompts/completions, key identity and Ori logs. A missing generation record is shown as `generationMetadataComplete: false` with a warning, not as a zero-cost claim; the overall `costProvenanceComplete` flag is false and the command exits nonzero after reporting. The output file is mode `0600` and should be retained only under the operator's approved report policy.

## Interpretation and follow-up

The initial four-case run passed both models on two synthetic cases each. That sample did not identify a quality winner. The requested `:free` candidate generations cost $0, while Ori made a paid TypeSafe Decisions API call for each case; the four ancillary calls cost $0.000248052. Ori's own `Served by` field mixes candidate and ancillary providers, so use the runner's separate provenance fields. Model behavior, upstream routing, ancillary calls, and prices can change; inspect fresh generation evidence for each run.

Review any candidate with representative, repeated cases and a live Kyber harness canary before applying it. MAT-106 tracks the harness conformance work. An API, console workflow, automatic model change, or scheduled rerun needs its own reviewed scope.
