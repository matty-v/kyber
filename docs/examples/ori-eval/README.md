# Synthetic Ori Eval fixture

This fixture supports the [MAT-109 workflow spike](../../design/2026-10-09-ori-eval-workflow-spike.md). It makes real OpenRouter calls, including an observed ancillary paid Decisions API request per case. It screens two models on two synthetic prompts; it does not exercise a Kyber agent or prove production suitability.

Run from a disposable directory with Ori `0.15.6+4855f79`, Bun `1.4.2`, and a dedicated `OPENROUTER_API_KEY` supplied by your secret manager. Review that key's OpenRouter budget before routine use. Keep the key out of shell history, files, logs, and PR artifacts. The `ORI_EVAL_OPT_IN=1` guard prevents accidental test execution.

```sh
eval_workdir=$(mktemp -d)
mkdir -p "$eval_workdir/evals" "$eval_workdir/features"
cp docs/examples/ori-eval/kyber-synthetic.eval.ts "$eval_workdir/evals/"
cd "$eval_workdir"

ORI_EVAL_OPT_IN=1 ori eval --list --allow-no-key ./evals
ORI_EVAL_OPT_IN=1 ori eval --dry-run --hermetic --no-history --features ./features ./evals

# Once the fixture, key, and cost policy are approved:
ORI_EVAL_OPT_IN=1 ORI_DISABLE_UPDATES=1 ORI_TELEMETRY=0 \
  ORI_FORCE_OPENROUTER_API_KEY=1 ORI_FALLBACK_MODELS='' ORI_MAX_TURNS=1 \
  timeout 180s ori eval --pilot 1 --hermetic --no-history \
  --features ./features --timeout 90000 ./evals
```

Inspect pilot generation records and billed cost before removing `--pilot 1` for the four-case run. Ori's pilot estimate and `toCostAtMost` assertion are observations, not pre-call spending limits. The empty features directory avoids copying repository-specific features into the temporary agent workspace. Ori still installs its own skills. After exporting approved aggregate results, remove the temporary workspace and inspect any Ori log/history locations for residual prompts.
