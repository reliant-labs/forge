---
name: choose-strategies
description: Pick go-to-market strategies deliberately — generate ideas across every category (content, community, outbound, partnerships, launches, paid, product-led, pricing, retention, referral, trust), filter by business type and model, score by effort, cost, time to first signal and fit, and avoid defaulting to the obvious channel.
---

# Choosing strategies

Teams usually pick the first channel they think of, or the one a competitor is loud on, and then wonder why it is slow. This skill replaces that with a short, explicit process: widen, filter, score, pick few.

Inputs: your positioning and facts list (`gtm/positioning`). Output: `strategies.md`, a ranked list with a reason for each decision, including the ones you rejected.

## Step 1: Generate broadly

Walk every category before judging anything. Aim for a few ideas per category, including ones that feel unlikely. If you keep an idea bank (a file of past and generic ideas by category), seed from it; if not, start from this list.

| Category | Examples of the kind of idea |
|---|---|
| Content | tutorials, teardown posts, comparison pages, a free tool or template, a changelog people read |
| Community and events | answering questions where your audience asks them, meetups, a talk, a hackathon prize, an office hours slot |
| Outbound and high-touch | founder-sent notes to a short list, concierge onboarding, a hand-delivered demo |
| Partnerships | integrations with products your users already use, co-written guides, marketplace listings, agency or consultant channels |
| Launches and PR | a launch on a relevant site or newsletter, a story only you can tell, a data post, a release with a real hook |
| Paid | search on high-intent terms, sponsored newsletters, retargeting, sponsorships |
| Product-led | a free tier or trial, a shareable output, in-product prompts, a better first-run experience |
| Pricing and offers | an annual plan, a starter bundle, a founder discount for early users, a pricing page rewrite |
| Retention and expansion | onboarding emails, usage nudges, win-back notes, a customer review cadence |
| Referral | an invite loop, a credit for both sides, a partner code |
| Trust | case studies with permission, security and status pages, public roadmap, reviews, a clear refund policy |

## Step 2: Filter by business type and model

Cut ideas that cannot work for you before scoring. Ask, in order:

1. **Who buys, and who decides?** A solo developer, a team lead and a procurement committee are reached very differently. High-touch outbound suits few, high-value buyers; product-led suits many self-serve ones.
2. **How does money move?**
   - *Subscription:* retention and expansion matter as much as acquisition; a churning customer erases the channel's payoff. Favor onboarding, annual plans, trust.
   - *Pay-as-you-go or usage-based:* the first successful use is the conversion. Favor free credits, quickstarts, templates and anything that shortens time to first working result.
   - *One-time purchase or license:* there is no recurring revenue to fund slow channels. Favor launches, content with a long tail, partnerships and referrals; plan new releases or add-ons as repeat purchase moments.
   - *Marketplace or two-sided:* you need both sides; pick the scarcer one and go narrow before wide.
   - *Open source with a paid tier:* community and docs are the funnel; do not gate the useful part.
3. **Where is the audience already?** An idea that needs the audience to go somewhere new is weaker than one that meets them where they are.
4. **What can you do consistently?** A weekly post you can sustain beats a monthly one you abandon.

## Step 3: Score

Score each surviving idea 1 to 5 on four dimensions, and write one line of reasoning per score. Be honest about low-confidence scores.

| Dimension | 5 means |
|---|---|
| Effort | a few hours of your time |
| Cost | free or negligible cash |
| Time to first signal | a result you can read within days |
| Fit | strong match with audience, model and positioning |

Sum or eyeball them, but also ask which idea would teach you the most if it failed. For a young product, fast signal usually outweighs a high ceiling: a cheap test that answers "does this audience care?" beats a big bet.

## Step 4: Pick few, and push back on the default

Choose two or three strategies for the next cycle, mixing speed (a quick signal) and compounding (something that keeps paying, like content or an integration). Then challenge the list:

- Is any pick just the channel everyone in your category uses? If so, what is the specific reason it will work for you, not for the category?
- Did the list include at least one idea from a category you rarely use?
- Would one of the rejected ideas beat a pick if the cost were lower? Can you shrink it into a cheap test?
- Is there a narrower audience you could win completely before going wide?

Record for each pick: the hypothesis ("we believe X audience will do Y if we do Z"), the first signal you will look for, and by when you will decide to continue or stop. This feeds `gtm/campaigns` and `gtm/measure`.

## Rules

- Any spend, outreach or public posting that comes from a strategy needs a human's approval before it happens. Choosing a strategy approves nothing.
- Do not score on invented market-size or conversion figures. If you lack data, say "unknown" and pick a test that produces it.
- Keep rejected ideas and the reason; they are the next cycle's starting point.

## When this skill is not enough

- Turning a pick into scheduled work: `gtm/campaigns`.
- Deciding what to measure: `gtm/measure`.
- Overall method: `gtm`.
