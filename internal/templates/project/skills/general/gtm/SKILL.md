---
name: gtm
description: Go-to-market methodology for a startup or app team — the loop from positioning to strategy choice, campaigns, content, outreach, events, measurement and weekly review, plus how to automate the recurring parts. Start here, then load the sub-skill for the step you are on.
---

# Go-to-market (GTM)

GTM is the work of getting the right people to find your product, understand it, try it and keep using it. This family of skills is a working method, not a channel playbook: it works the same for a subscription app, a pay-as-you-go API, a one-time purchase or an open-source project with a paid tier, and it assumes nothing about your tools.

## The loop

Run these in order the first time, then keep cycling. Each step has a skill; load it when you reach the step.

1. **Positioning** (`gtm/positioning`): who the product is for, what problem it solves, why you over the alternative, and a facts list with a source for every claim. Everything below draws on it.
2. **Choose strategies** (`gtm/choose-strategies`): generate ideas across all the categories, filter by business type and model, score, and pick a few. Resist the obvious channel.
3. **Campaigns** (`gtm/campaigns`): turn a chosen strategy into one goal, a set of pieces across channels, and a calendar.
4. **Pieces and channels**: write and run the work itself. `gtm/content` for blogs, short-form and video; `gtm/outreach` for founder-led and cold messages; `gtm/events` for meetups, conferences and hackathons.
5. **Measure** (`gtm/measure`): one north star, a few KPIs, and a small-sample-aware reading of what happened.
6. **Review** (also `gtm/measure`): a weekly look at results that ends in decisions: keep, change, stop, try next. The decisions feed back into positioning and strategy.

`gtm/automate` cuts across the loop. Event discovery, competitor monitoring, outreach research and metrics digests recur, so design them once as automations instead of redoing them by hand.

## Rules that apply to every skill here

- **A human approves every outward action.** Sending a message, publishing a post, registering for or sponsoring an event, posting in a community, spending on ads: an agent or script may research, draft and queue, and a person presses the button. Make this a gate in any automation you build, not a habit.
- **Never state an unverified claim.** Numbers, customer names, comparisons and "first/only/fastest" all need a source in the facts list before they appear in public. If there is no source, drop the claim or soften it to something you can show.
- **Write plainly.** No hype words, no invented statistics, no fake urgency, no pretending a template is a personal note.
- **One goal at a time.** A campaign or an experiment with three goals teaches you nothing about any of them.
- **Respect the audience.** Do not scrape what you may not use, email people who have opted out, or post where the community's rules forbid promotion.

## Keep the state in files

Agents lose context between sessions and people lose it between quarters, so keep the loop's artifacts in the repo (for example `docs/gtm/`): `positioning.md`, `facts.md`, `strategies.md`, one file per campaign, and one dated file per weekly review. Plain markdown or CSV is enough. Each skill says which file it reads and writes.

## Where to start

| You have | Load |
|---|---|
| No written positioning, or it is vague | `gtm/positioning` |
| Positioning, but no plan | `gtm/choose-strategies`, then `gtm/campaigns` |
| A campaign and need the pieces | `gtm/content`, `gtm/outreach`, `gtm/events` |
| Activity but no idea what works | `gtm/measure` |
| A task you keep repeating by hand | `gtm/automate` |

## When this skill is not enough

These skills structure the thinking and the paper trail; they do not replace talking to customers. If positioning is guesswork, interview five users before anything else. For legal questions on marketing to a given country or sector, ask a lawyer.
