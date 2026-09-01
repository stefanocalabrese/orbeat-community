# orbeat, Community edition

orbeat is a self-hosted catalog and gateway for AI-agent capabilities. AI coding
tools sign in once through your identity provider and get governed access to the
MCP servers you publish, plus distribution of shared skills, subagents and rules
onto developer machines.

## What you get, and where it stops

This edition runs the whole distribution path: OIDC sign-in, the catalog, the MCP
gateway, and all three ways a capability reaches a developer, which are the
Claude Code marketplace plugin, the `orbeat-sync` client, and the gateway itself.
Three limits mark where it ends:

- **10 active MCP servers.** Only servers whose status is `active` count, so
  disabling one frees its slot immediately.
- **1 custom role.** Entitlements are granted per role, so everyone you entitle
  sees the same catalog.
- **10 active seats, over a 7-day window.** A seat belongs to whoever signed in
  during those 7 days and is released on its own afterwards.
  `DELETE /v1/admin/users/{id}` releases one immediately.

Cross a limit and the API answers 402 with the cap, the current count and a
contact address. Nothing already running is disturbed.

**There is no approval workflow.** An artifact is approved the moment an admin
saves it, and the next `orbeat-sync` run puts it on developer machines. Writes
are still validated for type, frontmatter and a 64KiB content limit, but the
scanner that looks for leaked secrets and reserved markers runs at submit, which
belongs to the approval workflow, so it never runs here. If you want a reviewer
standing between an author and a developer's machine, that is the Enterprise
edition, which also has revision history and rollback, audit export, `vault:` and
`awssm:` secret references, and no caps.

## What is here

- `orbeat-gateway`, a Streamable-HTTP MCP gateway that brokers your upstream MCP
  servers behind one OAuth 2.1 endpoint, with per-call authorization.
- `orbeat-api`, the catalog, entitlements and audit API.
- `orbeat-portal`, the self-service and admin console.
- `orbeat-sync`, a CLI that reconciles entitled artifacts into a developer's
  local tools.

Requires Go 1.26, Node 24 and Docker. `make up` starts the stack.

## Rough edges

The admin console is the Enterprise console with its Enterprise-only pages still
in it, so parts of it lead nowhere.

- **Review queue** always reads "Nothing pending review". Nothing can be pending
  when everything is approved on write, so the page stays empty for good.
- **Withdraw** and **History** sit on every row of the Artifacts page, and
  **Export JSON** and **Export CSV** on the Audit page. All four call endpoints
  this edition does not register, so they answer 404.

`openapi.yaml` still lists the 27 Enterprise-only endpoints this edition does not
register. No documentation ships in this tree.

## Licence

Apache-2.0. See `LICENSE` and `NOTICE`.

## Contributing

This tree is generated from another repository and is overwritten on every
release, so changes made here are lost. Open issues rather than pull requests.

Module: github.com/stefanocalabrese/orbeat-community
