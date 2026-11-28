---
name: campaigns
description: Plan a go-to-market campaign — one goal, the strategy it tests, several channels and formats, planned-versus-drafted coverage tracking, and a calendar that a small team can keep to.
---

# Campaigns

A campaign is a bounded push toward one goal, run through a few channels over a few weeks. It is the unit that connects a chosen strategy (`gtm/choose-strategies`) to the actual pieces (`gtm/content`, `gtm/outreach`, `gtm/events`) and to the result you will read (`gtm/measure`).

## One goal per campaign

State a single goal with a number and a date, in terms of something you can observe: "30 signups from the founder community by 15 March", "10 booked calls with agencies this month", "5 integrations installs from the marketplace listing". If you cannot name how you will count it, the goal is not ready.

Everything else is secondary: awareness, followers and traffic are inputs. Put a second goal in a second campaign, or you will not be able to tell what worked.

## The campaign file

Keep one file per campaign (`docs/gtm/campaigns/<name>.md`):

| Field | Content |
|---|---|
| Goal | the one number and the date |
| Hypothesis | what you believe and why, from the strategy it came from |
| Audience | which positioning block it addresses |
| Offer and call to action | the single next step you ask people to take |
| Channels and formats | the table below |
| Claims | the fact rows the campaign relies on (`gtm/positioning`) |
| Owner and approver | who is responsible and who approves outward actions |
| Budget | cash and hours, with a hard stop |
| Decision date | when you will decide to continue, change or stop |

One call to action keeps the measurement clean. Give every channel its own tracked link, for example a distinct `utm_campaign` and `utm_source`, so signups can be attributed without guessing.

## Several channels and formats

A single post is a thin campaign. Cover the goal through a few channels with different formats, each adapted to the place it appears, not copy-pasted:

- a long piece that holds the substance (post, guide, demo page)
- short pieces that point to it (posts, threads, clips)
- direct messages to a short list of people who would care
- a presence where the audience gathers (a community answer, a meetup talk, a newsletter slot)
- an owned channel that captures the response (landing page, email sequence, in-product message)

Track coverage in a table, one row per piece:

| Piece | Channel | Format | Status | Date | Fact rows | Approved by |
|---|---|---|---|---|---|---|
| Launch post | blog | long-form | drafted | 3 Mar | F2, F7 | pending |
| Summary thread | social | short-form | planned | 4 Mar | F2 | |
| Community answer | forum | reply | planned | rolling | F7 | |

Statuses: `planned`, `drafted`, `approved`, `published`, `measured`. The planned-versus-drafted view shows at a glance where the campaign is thin: a channel with nothing drafted, or a goal that depends on a piece nobody owns. Sort by date and you have a working calendar.

## Calendar

Keep it small enough to hold in your head: one table sorted by date, with owners. Rules of thumb:

- put the anchor piece first and schedule the rest around it
- space posts to what you can actually produce; do not plan daily output for a team of one
- leave a slot for a reactive piece, because something relevant will happen
- check each date against events, holidays and releases that could clash
- if a date slips, move it in the file; a stale calendar is worse than none

## Run it

1. Write the campaign file and have the owner approve the goal and budget.
2. Draft the pieces, run the claims check, and queue them.
3. A human approves each outward action before it goes out: every publish, send, registration and payment.
4. Update statuses as you go, and note anything that diverged from the plan.
5. At the decision date, read the result (`gtm/measure`) and record what you will keep, change or stop.

## Rules

- Do not start a campaign without a goal, an owner and an approver.
- No spend beyond the stated budget, and no spend without approval.
- Do not run overlapping campaigns on the same audience and channel unless you can attribute each separately.

## When this skill is not enough

- Repeating parts of a campaign on a schedule, such as weekly roundups or monitoring: `gtm/automate`.
- Overall method: `gtm`.
