---
name: measure
description: Measure go-to-market work honestly — pick a north star and a few KPIs, read product analytics with attribution in mind, treat small samples with caution, and run a weekly review that ends in keep, change, stop or try decisions.
---

# Measure and review

Measurement answers one question: is what we are doing moving the thing we care about? Most early-stage teams have too few data points for statistics and too many dashboards for attention. The aim is a small set of numbers, read plainly, that change what you do next week.

## Pick a north star

Choose one metric that reflects customers getting value and that you can influence, and write it in the repo with its exact definition. It should move when the business is healthier and not when you merely make noise.

Examples by model, to adapt rather than copy:

| Model | A north star might be |
|---|---|
| Subscription | active paying accounts that completed the key action this month |
| Pay-as-you-go | accounts that reach a first successful use within a set time, then repeat |
| One-time purchase | purchases per month, with refund rate beside it |
| Open source with paid tier | active installs or projects, plus paid conversions |
| Marketplace | completed transactions with both sides present |

Avoid vanity metrics as the star: page views, followers and raw signups can all rise while the business does not.

## Add a few KPIs

Add three to six measures that lead to the star, in the order a person moves through them: reach, visit, signup, activation (the first real value), retention, revenue. Define each precisely, including what counts and the time window. Note which are leading (move quickly) and which are lagging (move slowly).

Track each campaign against its one goal (`gtm/campaigns`) and, separately, the product-wide north star.

## Read product analytics carefully

- **Attribute with tracked links.** Use consistent campaign tags on every link you control. "Direct" and "unknown" traffic usually hides word of mouth and dark social; do not credit it to the last thing you posted.
- **Check instrumentation first.** Before believing a number, confirm the event fires, fires once and is not blocked or filtered. A funnel with a missing step looks like a conversion crisis.
- **Compare like with like.** Same window, same audience, same definition. Beware weekday effects, launches and holidays.
- **Use cohorts for retention.** Look at groups by signup week rather than a blended average that hides whether newer users stay.
- **Pair quantity with quality.** Signups from a channel that never activate are not a win; follow each channel to activation and to revenue.
- **Respect privacy.** Collect only what you need, honour consent and do-not-track choices, and avoid putting personal data into shared reports or prompts.

## Small-sample caution

Early numbers are small and noisy. Ten signups versus seven is not a result.

- Say how many observations sit behind every percentage. "50% conversion (2 of 4)" reads very differently from "50%".
- Do not declare a winner from a handful of events, or from a difference smaller than week-to-week variation. Wait for more data or run the test longer.
- Look for consistency across weeks before trusting a change.
- Prefer decisions that are cheap to reverse when evidence is thin, and save big commitments for repeated signals.
- Do not hunt through many slices of the data until something looks significant; decide what you will look at before you look.
- Qualitative evidence counts: five replies that say the same thing are information. Quote them, with the reader's permission if it will be shared.
- Never write up a number you cannot reproduce from a stated query or export. Same rule as the facts list (`gtm/positioning`).

## Weekly review

Keep it to thirty minutes, at a fixed time, with the same template. Save one dated file per review (`docs/gtm/reviews/2025-03-10.md`).

1. **Numbers:** north star and KPIs this week versus last week and four weeks ago, each with its sample size and the query or source.
2. **Campaigns:** for each active campaign, progress against its goal and decision date.
3. **What we learned:** two or three plain sentences, including surprises and anything that contradicts the positioning.
4. **Decisions:** for each active effort, one of **keep**, **change** (say what), **stop**, or **try** (a new test from the strategy list). Give an owner and a date.
5. **Claims and facts:** update any fact row whose number or status changed.
6. **Next week's approvals:** list the outward actions waiting for a human's approval.

Feed decisions back: a stopped strategy goes into the rejected list with the reason; a surprise may change positioning.

## Rules

- Report only numbers you can source; mark estimates as estimates.
- A review recommends; a human decides on spending, pausing or changing a public message.
- Do not move the goalposts after the result: record the goal before the campaign, and note any change and why.

## Automating it

Gathering last week's numbers into a draft digest is a good recurring job. The design for a weekly metrics digest, with its inputs, approval point and failure handling, is in `gtm/automate`. Automation drafts the digest; a person still makes the decisions.

## When this skill is not enough

- Setting up analytics itself, such as event tracking and dashboards, is product engineering work outside this skill.
- For a rigorous experiment on a lot of traffic, use a proper experimentation tool and a statistician's advice.
- Overall method: `gtm`.
