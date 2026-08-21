# Kyber Product Docs

This tree is the **product source of truth** for Kyber: what the product does,
what an operator sees and can do, and what its states and concepts mean. It is
written for someone evaluating or running Kyber, not for someone hacking on it.

These pages are published twice from this single source:

- **In-repo**, right here, rendered by GitHub.
- **On the docs site, [kyber.voget.io](https://kyber.voget.io)**, which mirrors
  this tree at build time. The site's build pulls this directory from `main`,
  so a merged change here is live on the site within minutes (a
  `repository_dispatch` from `.github/workflows/notify-site.yml` triggers the
  rebuild, with a scheduled rebuild as the safety net).

[`manifest.json`](manifest.json) is the publication contract: it lists the
sections, page order, and sidebar labels the site renders. A page not in the
manifest does not publish. Adding a page means adding the file **and** a
manifest entry.

## Structure

| Section | What belongs there |
|---|---|
| [`getting-started/`](getting-started/what-is-kyber.md) | What Kyber is, the quickstart, and the installation options |
| [`capabilities/`](capabilities/fleet-console.md) | One page per capability area: what it does and how an operator uses it |
| [`use-cases/`](use-cases/README.md) | Narrative walkthroughs of ways to run Kyber |
| [`project/`](project/architecture.md) | Product-level architecture, the security model, and the FAQ |

## Scope: WHAT, not HOW

These pages describe **what the product does**. They deliberately contain no
implementation detail: no file paths, function names, or controller internals.
That is the HOW, and it lives in the sibling set,
[`../architecture/`](../architecture/overview.md). The two sets cross-link so
the boundary is navigable from either direction.

## Writing rules

- First line is the `# H1` title; the paragraph after it is the page's lead
  (the site uses it as the meta description). No YAML frontmatter.
- Links to pages inside this tree are relative `.md` links (the site rewrites
  them to site routes). Links to anything else in the repo are relative paths
  too (the site rewrites those to GitHub). Never hardcode a kyber.voget.io URL.
- Plain voice, no em dashes.
- Release-version pins in `getting-started/quickstart.md` are stamped
  automatically by `prepare-release.yml` at each release; write them as the
  current bare semver and let the release flow keep them fresh.
- `product_docs_test.sh` checks the structural invariants (manifest/page
  agreement, the WHAT/HOW boundary, no em dashes). Run it from the repo root
  after editing.
