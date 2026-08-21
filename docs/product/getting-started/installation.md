# Installation options

Kyber installs from a published Helm chart on any Kubernetes cluster. Pick the
path that matches where you want it to run, from a local test cluster to a
production GCP deployment. Each path links to its canonical step-by-step guide.

## Choose your path

**Try it on a cluster you already have, or a local one.** The
[quickstart](./quickstart.md) takes any cluster (or a k3d one-liner) to a
running fleet console with one live agent in about 15 minutes, with a verify
step at every stage.

**Have an AI assistant do the install for you.** You do not have to be a
developer. The
[install with an AI assistant guide](../../install-with-an-ai-assistant.md)
gives you a prompt to paste into Claude Code, Cursor, or ChatGPT that drives
the install carefully: plain-language narration, a verify step before moving
on, and a pause whenever a step needs you.

**Run it on your Mac.** The [macOS guide](../../installation-macos.md) runs the
cluster inside a Linux VM on your Mac. Read its support table first: it covers
Intel and Apple Silicon, and tells you which releases ship native `arm64`
images and when the VM must be `x86_64` instead.

**Run it on a Windows laptop, no cloud account.** The
[WSL2 guide](../../installation-wsl2.md) is a numbered runbook for a standalone
single-box install: native k3s in WSL2 and Tailscale Funnel for a public HTTPS
URL.

**Run it on GCP with managed VMs and HTTPS.** The
[GCP guide](../../installation.md) is the production install:
Terraform-provisioned VMs, numbered steps from secrets to first agent, and a
public HTTPS URL.

## A note on trust

Agent pods are unprivileged and run in a Linux user namespace, so in-pod root
maps to an unprivileged uid on the node. They keep `CAP_SYS_ADMIN` inside that
namespace to assemble each agent's whole persistent disk; the capability
carries no authority on the node. Still, give Kyber a dedicated cluster you
are comfortable trusting the agents with, not a shared production one. The
[threat model](../../../.github/SECURITY.md#deployment-threat-model) explains
why.

## Learn more

- [The install table in the docs index](../../README.md)
- [Upgrading an existing install](../../upgrading.md)
- [Security policy and threat model](../../../.github/SECURITY.md)
- [What is Kyber?](./what-is-kyber.md)
