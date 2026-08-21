# Install Kyber with an AI assistant

You don't have to be a developer to install Kyber. Open the AI assistant of your
choice (Claude Code, Cursor, ChatGPT, …), fill in your install target on the
second line, paste the whole prompt below, and follow what it tells you to do.

The prompt points the assistant at the real install guides and sets ground rules
so it drives the install carefully: plain-language narration, a verify step
before it moves on, and a pause whenever a step needs you (a browser login, a
token, an approval).

```
You're helping me install Kyber. Kyber is a self-hosted AI agent fleet
platform. Repo: https://github.com/matty-v/kyber.
My install target: <one of: my Mac / WSL2 on my Windows laptop /
a GCP project / a Kubernetes cluster I already have>

Read the guide for my target end-to-end before starting. Your job is to
drive the install, not improvise one:
- Any existing cluster, or just trying Kyber out: docs/product/getting-started/quickstart.md,
  the shortest path, ~15 minutes, one helm install and a first agent.
  Start here if you are unsure.
- macOS: docs/installation-macos.md, which starts a Linux VM with
  Kubernetes on the Mac and then hands off to the quickstart. Read its
  support table first: on Apple Silicon the VM must be x86_64.
- WSL2 (Windows laptop): docs/installation-wsl2.md, a numbered runbook
  with explicit verify steps and recovery paths. Start at § 0, proceed
  sequentially.
- GCP: docs/installation.md, numbered steps from secrets through
  Terraform provisioning to first agent and an HTTPS URL.

Operating principles:
1. I'm not a developer. Before each step, tell me in one plain-language
   sentence what you're about to do.
2. Where the guide gives a verify command, don't proceed until it produces
   the expected output. If it fails, follow the guide's recovery pointer.
   Don't improvise.
3. Some steps need me (browser flows, PAT generation, OAuth approval). When
   you hit one, pause, tell me exactly what to do with the URL, and wait for
   me to come back with the value. Never invent a credential.
4. Don't paste secrets into our chat or commit them. They go into shell
   variables or files at paths the guide specifies.
5. Anything destructive (rm -rf, wsl --unregister, terraform destroy,
   k3d cluster delete), confirm with me first.
6. If this is a resumed session and you've lost context, re-run the guide's
   verify commands from the top. Your starting point is the first one
   that fails.
```

## Doing it yourself instead

- [Quickstart](product/getting-started/quickstart.md): any Kubernetes cluster (or a k3d one-liner) to a
  running fleet console with one live agent
- [macOS install guide](installation-macos.md): a Mac, via a Linux VM with
  Kubernetes
- [WSL2 install guide](installation-wsl2.md): a Windows laptop, no cloud account
- [GCP install guide](installation.md): production multi-VM install with HTTPS
- [Local dev stack](../scripts/devenv/full-local.md): for hacking on Kyber
  itself, from your working tree
