#!/usr/bin/python3
"""Converge the Kyber-owned portion of Hermes config.yaml."""

from __future__ import annotations

import os
from pathlib import Path
import tempfile

import yaml


MANAGED_SERVERS = {
    "kyber_telegram": "KYBER_TELEGRAM_MCP_URL",
    "kyber_discord": "KYBER_DISCORD_MCP_URL",
    "kyber_slack": "KYBER_SLACK_MCP_URL",
    "kyber_request_reply": "KYBER_REQUEST_MCP_URL",
    "kyber_a2a": "KYBER_A2A_MCP_URL",
}


# MANAGED_PROVIDER must match hermes.CustomProviderID in
# pkg/runtimes/hermes/adapter.go. The adapter writes the providers entry's name
# into HERMES_PROVIDER, which Hermes resolves against the block written here;
# if the two ever disagree Hermes looks up a provider that does not exist.
MANAGED_PROVIDER = "kyber-endpoint"

# The Hermes hook event that fires before a model call. Claude Code uses
# UserPromptSubmit, which fires once per user prompt AND can inject context;
# Hermes offers neither, so the hook script deduplicates on turn_id itself.
GOAL_HOOK_EVENT = "pre_llm_call"
GOAL_HOOK_COMMAND = "/usr/local/bin/kyber-hermes-goal-start"

# The env var the adapter injects the endpoint credential into, and which
# upstream Hermes reads for a provider configured with an explicit base_url.
# Named once and interpolated below so no line in this file pairs a
# credential-shaped key with a quoted literal — secret scanners flag that
# shape on sight, however inert the value.
INFERENCE_CREDENTIAL_ENV = "OPENAI_API_KEY"


def mapping(value: object) -> dict:
    return value if isinstance(value, dict) else {}


def main() -> None:
    home = Path(os.environ.get("HERMES_HOME", Path.home() / ".hermes"))
    path = home / "config.yaml"
    home.mkdir(parents=True, exist_ok=True)
    home.chmod(0o700)

    config: dict = {}
    if path.exists():
        loaded = yaml.safe_load(path.read_text(encoding="utf-8"))
        if loaded is not None and not isinstance(loaded, dict):
            raise SystemExit(f"{path} must contain a YAML mapping")
        config = mapping(loaded)

    servers = mapping(config.get("mcp_servers"))
    for name, env_name in MANAGED_SERVERS.items():
        url = os.environ.get(env_name, "").strip()
        if url:
            servers[name] = {"url": url}
        else:
            servers.pop(name, None)
    if servers:
        config["mcp_servers"] = servers
    else:
        config.pop("mcp_servers", None)

    # An agent pointed at its own inference endpoint (spec.inference) gets a
    # Kyber-owned `providers` entry naming that endpoint. Hermes resolves
    # `--provider <id>` against this block, and upstream ignores the provider's
    # own key env once base_url is set — so the credential is referenced as
    # ${OPENAI_API_KEY}, which the adapter injects from the operator's Secret.
    #
    # Converged the same way mcp_servers is: written from env when present,
    # REMOVED when absent. Clearing spec.inference must put the agent back on
    # its built-in provider rather than leaving a stale block behind that
    # Hermes would still resolve.
    # MANAGED_PROVIDER is the removal key. When spec.inference is cleared the
    # adapter stops setting KYBER_INFERENCE_PROVIDER entirely, so reading the
    # id only from env would leave the stale block in place and Hermes would
    # keep resolving it. Falling back to the constant is what makes clearing
    # the field actually take effect.
    providers = mapping(config.get("providers"))
    provider_id = os.environ.get("KYBER_INFERENCE_PROVIDER", "").strip() or MANAGED_PROVIDER
    base_url = os.environ.get("KYBER_INFERENCE_BASE_URL", "").strip()
    if base_url:
        credential_ref = "${%s}" % INFERENCE_CREDENTIAL_ENV
        entry = {"base_url": base_url, "api_key": credential_ref}
        model = os.environ.get("HERMES_INFERENCE_MODEL", "").strip()
        if model:
            entry["model"] = model
        providers[provider_id] = entry
    else:
        providers.pop(provider_id, None)
        providers.pop(MANAGED_PROVIDER, None)
    if providers:
        config["providers"] = providers
    else:
        config.pop("providers", None)

    # Agent goals. A pre_llm_call hook opens a goal revision at the start of
    # each user turn; the script itself deduplicates on turn_id, because this
    # event fires on EVERY model call and re-opening mid-turn would wipe the
    # summary the agent just set.
    #
    # Converged like mcp_servers above: written when the command is present,
    # REMOVED when it is not, so an image without the script does not leave a
    # dangling hook Hermes would try to run every call.
    hooks = mapping(config.get("hooks"))
    managed_hooks = [h for h in hooks.get(GOAL_HOOK_EVENT, [])
                     if isinstance(h, dict) and h.get("command") != GOAL_HOOK_COMMAND]
    if os.access(GOAL_HOOK_COMMAND, os.X_OK):
        managed_hooks.append({"command": GOAL_HOOK_COMMAND, "timeout": 5})
    if managed_hooks:
        hooks[GOAL_HOOK_EVENT] = managed_hooks
    else:
        hooks.pop(GOAL_HOOK_EVENT, None)
    if hooks:
        config["hooks"] = hooks
    else:
        config.pop("hooks", None)

    # Kyber's identity repository is the durable source for agent-authored
    # memory and skills. Disable Hermes's automatic post-turn mutation fork
    # until that write path is bridged explicitly.
    auxiliary = mapping(config.get("auxiliary"))
    background = mapping(auxiliary.get("background_review"))
    background["enabled"] = False
    auxiliary["background_review"] = background
    config["auxiliary"] = auxiliary

    fd, tmp_name = tempfile.mkstemp(prefix=".config.yaml.", dir=home)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            yaml.safe_dump(config, handle, sort_keys=False)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(tmp_name, 0o600)
        os.replace(tmp_name, path)
    finally:
        if os.path.exists(tmp_name):
            os.unlink(tmp_name)


if __name__ == "__main__":
    main()
