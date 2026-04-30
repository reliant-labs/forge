# Architecture Decision Records

This directory holds Architecture Decision Records (ADRs) using the
[MADR](https://adr.github.io/madr/) template (Markdown Architectural
Decision Records).

## What is an ADR?

A short, immutable document that captures one architectural decision
and the context that led to it. ADRs are append-only: once a decision
is accepted, we don't edit it — we supersede it with a newer ADR.

## Writing a new ADR

1. Copy `0001-use-connect-rpc.md` to `NNNN-short-title.md` where `NNNN`
   is the next zero-padded number.
2. Fill in **Context**, **Decision**, and **Consequences** (minimum).
   Add **Considered Options** when there's a real tradeoff worth
   recording.
3. Set `status: proposed` while you're iterating. Flip to `accepted`
   when the team signs off, `superseded by NNNN` when a later ADR
   replaces it.
4. Link the ADR from relevant code comments or docs. The filename is
   the stable identifier.

## Template

See [MADR 3.0](https://adr.github.io/madr/) for the full template and
reasoning. The key sections we expect:

- **Status** — `proposed` | `accepted` | `superseded by NNNN`
- **Context and Problem Statement** — what forces are at play?
- **Decision Drivers** — what we're optimizing for.
- **Considered Options** — at least one alternative (even "do nothing").
- **Decision Outcome** — chosen option + rationale.
- **Consequences** — good and bad.
