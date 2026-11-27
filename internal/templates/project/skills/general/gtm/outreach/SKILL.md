---
name: outreach
description: Founder-led and high-touch outreach plus respectful cold outreach — build a researched shortlist, personalise honestly, keep volume low, honour opt-outs, and know the high-level CAN-SPAM and GDPR basics. Every message is sent only after a human approves it.
---

# Outreach

Outreach is one person writing to another person because they have a problem you can plausibly help with. It works when the list is short, the research is real and the message is easy to say no to. It fails when it is a mail merge in disguise.

Two modes share the same discipline:

- **Founder-led and high-touch:** a handful of people you actually want as customers, partners or advisors, contacted by the founder or a named team member, often leading to a call or a hands-on onboarding.
- **Cold outreach:** a larger list of strangers who fit your audience but have never heard of you. It can work, but it deserves more care, not less.

## Step 1: Build a shortlist

Start from positioning (`gtm/positioning`): who is the audience, and what signals suggest a given person has the problem now? Good signals are public and specific: they posted about the problem, hired for the role that owns it, shipped something your product would help with, or use a tool yours integrates with.

Keep the list small, 10 to 30 to start. For each person record: name, role, company, why now (the signal and its link), the one thing you would offer, and where you found their contact details. If you cannot say why you are writing to this person specifically, remove them.

Use only sources you may use. Respect site terms and robots rules, do not scrape private data, and do not guess or buy personal email addresses in bulk.

## Step 2: Research and personalise honestly

Spend a few minutes per person reading their recent public work. Then write one or two sentences that only make sense for them: what you noticed, and why you are connecting it to your product. Personalisation is a reason, not a mail-merge field with their company name.

Do not fake familiarity ("loved your recent post" with no detail), invent mutual connections, or pretend the message was not templated when it was. Do not claim anything about your product that is not in the facts list.

## Step 3: Write the message

Keep it short enough to read on a phone:

1. why you are writing to them, in one sentence
2. what you offer, in one sentence, with the claim sourced from your facts list
3. one small, specific ask that is easy to decline ("worth a 15-minute look?", "can I send the demo link?")
4. who you are, and a plain way to say no or stop hearing from you

No attachments in a first note, no tracking-pixel tricks, no false "Re:" subject lines, no fake urgency. One follow-up after several days is reasonable; a long drip sequence to someone who has not replied is not.

## Step 4: Limits on volume

- Send in small batches and look at replies before sending more. Many tens of messages a day from a new address is a spam pattern and damages deliverability for everyone on the domain.
- One touch plus at most one or two follow-ups per person, spaced out. Then stop.
- A bounce, a "not interested" or an unsubscribe ends contact immediately.
- Keep cold email off your main product domain's transactional sending if you can, so a complaint does not break password resets.

## Opt-out and legal basics

This is a high-level orientation, not legal advice. Rules vary by country and by whether the recipient is a person or a company; ask a lawyer before scaling.

- **CAN-SPAM (US):** commercial email must not use deceptive headers or subject lines, must identify itself as an ad where applicable, must include a valid physical postal address, and must give a clear way to opt out that you honour promptly.
- **GDPR and ePrivacy (EU and UK):** processing personal data needs a lawful basis; cold business email is treated differently in different countries, and consent is often required for individuals. Tell people where you got their details, why you are writing, and how to object, and honour erasure and objection requests.
- **Other places:** Canada, Australia and many other countries have their own consent rules; some are stricter than the US.
- **Channels beyond email:** direct messages on social and community platforms are governed by that platform's rules as well as the law; messaging people in communities that forbid promotion gets you banned and hurts your name.
- **Keep a suppression list:** anyone who opts out or objects goes on it, and the list is checked before every send, including automated ones.

## Run it

1. Build the shortlist and the sourced reasons (this part can be automated; see `gtm/automate`, "Outreach research shortlist").
2. Draft each message; the drafter notes which fact rows it relied on.
3. **A human reads and approves each message, or each batch, before anything is sent.** Automation may research and draft; it must not send.
4. Send in small batches from a real, named sender who can answer replies.
5. Log outcomes: sent, replied, positive, negative, opted out. Update the suppression list.
6. Review weekly (`gtm/measure`), and change the list or message when replies are low rather than increasing volume.

## Rules

- No sending without human approval, including follow-ups.
- No bulk-bought lists, no scraped personal data, no deceptive framing.
- Honour every opt-out, and check the suppression list first.
- Reply to replies, including no's, politely and quickly.

## When this skill is not enough

- Recurring research and shortlisting as an automation: `gtm/automate`.
- Meeting people in person: `gtm/events`.
- Overall method: `gtm`.
