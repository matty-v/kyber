#!/usr/bin/env python3
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
