---
name: positioning
description: Write product positioning and a sourced facts list for go-to-market work — audience, problem, alternatives, differentiation, proof — plus claim discipline so no unverified number, comparison or customer name ever reaches public copy.
---

# Positioning and facts

Positioning is a short, falsifiable statement of who the product is for and why they would pick it. A facts list is the evidence behind it. Together they are the source every campaign, post and email draws from, so errors made here are repeated everywhere.

## Step 1: Gather what you actually know

Before writing, collect inputs from reality, not from what you wish were true:

- the product itself: what it does today, not the roadmap
- how current users describe it, in their words (support threads, sales calls, reviews)
- who actually signs up, pays and stays, and who tried and left
- the real alternatives, including "a spreadsheet" and "do nothing", not just named competitors

If you have no users yet, say so in the file. Positioning for a product with no users is a hypothesis, and the first campaigns are how you test it.

## Step 2: Write the positioning

Fill in each line in one or two plain sentences. If a line takes a paragraph, you have not decided yet.

| Line | Question it answers |
|---|---|
| Audience | Who, specifically, has this problem? Role, company type, situation. "Developers" is too wide; "solo founders shipping a SaaS on a weekend budget" is usable. |
| Problem | What are they doing now, and what does it cost them in time, money or risk? |
| Alternative | What would they do if your product did not exist? |
| Difference | What do you do that the alternative does not, and for whom does that matter? |
| Proof | What can you show: a demo, a number you measured, a customer who agreed to be named? |
| Not for | Who should not buy it? Saying this builds trust and saves support time. |

Then write one sentence from it: "For <audience> who <problem>, <product> is <category> that <difference>, unlike <alternative>." Read it to someone outside the team. If they cannot repeat it back, simplify.

Keep one positioning per audience. A product with two very different buyers has two positioning blocks that share a facts list.

## Step 3: Build the facts list

Create `facts.md` (or a table) where every public-facing claim is one row:

| Claim | Type | Source | Checked by / date | Allowed wording |
|---|---|---|---|---|
| "Deploys in one command" | capability | link to docs or a recorded run | name, date | as written |
| "Used by 40 teams" | number | analytics query or export | name, date | "used by dozens of teams" |
| "Faster than X" | comparison | your benchmark, method and date | name, date | only with the method stated |

Types worth separating: capability, price or limit, number, comparison, customer or quote, and security or compliance statement. The last two carry the highest risk; a customer name needs their permission on record, and a compliance claim needs the certificate or report.

## Claim discipline

- **No source, no claim.** A statement that is not in the facts list does not appear in public copy.
- **Numbers carry a date and a method.** A figure that was true in March may not be true now; recheck before reuse.
- **Comparisons need the method and the competitor's current version.** Competitors change; stale comparisons are the most common way a team ends up correcting itself in public.
- **Quotes and logos need written permission.** Keep the permission with the row.
- **Prefer a demonstrable claim to a superlative.** "Sets up in under ten minutes, here is the recording" beats "the easiest tool".
- **Re-verify on a schedule.** Add a "next check" date to rows that can drift: pricing, limits, user counts, integrations.

Anyone drafting copy (a person or an agent) states which fact row each claim comes from. Anything without a row is flagged for a human, not published.

## Rules

- Positioning is a draft until a human owner approves it; agents propose edits, they do not rewrite it silently.
- Do not invent customers, testimonials, metrics or roadmap dates to fill a gap. Leave the gap visible.
- Keep it short: one page for positioning, a table for facts.

## When this skill is not enough

- Choosing what to do with the positioning: `gtm/choose-strategies`.
- Checking claims in a specific draft: the claims check in `gtm/content`.
- Overall method: `gtm`.
