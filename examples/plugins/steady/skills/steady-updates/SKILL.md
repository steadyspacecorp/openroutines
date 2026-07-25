---
name: steady-updates
description: Use when writing, drafting, improving, or reviewing a Steady check-in or goal update (goal story update) — whether as a writing aid for a person or when an agent composes an update autonomously. Covers what makes an update clear and useful — adding context beyond captured activity, right length, unambiguous people references, and scannable formatting. Trigger when the user asks to write/draft/polish a check-in, "previous"/"intentions"/"blockers", a goal update, a progress story, or a standup-style update for Steady — or when an agent needs to report progress into Steady.
---

# Writing great Steady updates

A good update gives teammates **signal**, not a transcript. The whole test is clarity: would a teammate skimming this understand what happened, what's next, and what's at risk — without asking a follow-up question? Add words only when they add clarity; never pad to hit a length, never truncate to fit one. And be straight: name real blockers and risks plainly, and write only what you actually know — don't manufacture a rationale or a status you don't have.

This skill is about _what to write_. To submit it, see the `steady-cli` or `steady-api` skill, or the Steady MCP server.

## Two formats

- **Check-in** — periodic, tight. Fields: `previous` (what you did since last time), `intentions` (what's next), `blockers` (what's in your way), `mood`. Scannable bullets.
- **Goal update** — a story about a goal's progress. Fields: `title` (the milestone in a phrase), `body` (the narrative), `progress` (0–100), `confidence` (30 / 60 / 90 = off track / at risk / on track). It's expected to run longer than a check-in, and the structure matters as much as the length — see "Long-form updates" below. Longer is fine _when it's all signal_.

## Anti-patterns

### 1. Restating activity without adding context

Surfacing activity in the update is fine — often better than leaving it out. Steady's captured activity is **pull**: a teammate has to go look for it. Putting something in your update is **push**: you're proactively making people aware of it. So pushing the important stuff is worth doing _even when it's also in activity_.

The anti-pattern is pushing it **bare** — a bare reference makes the reader drill down for the point. Always carry the context with it, so the answer is right there.

> ✗ "Merged PR #2345." · "Worked on issue #ABC-123." · "Had a meeting with Sarah."
> ✓ "Merged PR #2345, which overhauls form validation across the key app views."
> ✓ "Spiked an approach to defer loading the support widget until first interaction. It was a smaller lift than expected since we were already using custom triggers."
> ✓ "Closed out #ABC-123. The root cause was a stale cache, so I added a guard to invalidate it on save."

### 2. Length that isn't scannable

Each check-in bullet should be **1–3 sentences** carrying one idea. Lead with the outcome, not the play-by-play. (Goal updates may be longer — but the same rule applies per bullet, and every sentence still has to earn its place.)

### 3. Ambiguous people references

A name with no handle and no surname is a dead end for the reader. Use an **@mention** (preferred — it links and notifies) or a full name + context.

> ✗ "Got feedback from the designer." · "Waiting on approval from John."
> ✓ "Got feedback from @maria." · "Waiting on approval from John Okafor (finance)."

### 4. Prose where bullets belong

Any enumerable set — multiple tasks, next actions, blockers, dependencies — reads better as a bulleted list than a run-on paragraph. Reserve prose for a single throughline (often the opening frame of a goal update).

### 5. Bare links instead of contextual ones

Anchor the link on the words that describe it, so the meaningful phrase is what's clickable. Don't write it out in plain text and tack a separate link on the end.

> ✗ "Worked on a PR to add action items to the API, with a narrow focus on check-in and goal update reminders. [link]"
> ✓ "Worked on a PR to [add action items to the API](https://github.com/...), with a narrow focus on check-in and goal update reminders."

## A good check-in, end to end

```
previous:
- Added CSV export, rolling it out behind a flag. It streams instead of buffering, which keeps memory flat on large accounts.
- Tracked down the flaky checkout test. It was a timezone assumption in the test itself, not a problem with the payment code.
- Synced with @maria on the billing edge cases. We're deferring proration to next cycle so it doesn't block launch.

intentions:
- Roll the export flag out to 10% and keep an eye on error rates.
- Start on webhook retries. Writing a short spec first before any code.

blockers:
- Waiting on @sarah to sign off on the proration deferral before I can close the ticket.
```

## Long-form updates: inverted pyramid

A goal update (or any longer update) should follow the **inverted pyramid**: lead with the bottom line, then the supporting detail, then the background last. State the outcome up front — don't build up to it. A reader who stops after the first line should still walk away with the headline; a reader who wants the full story keeps going.

```
title: Self-serve onboarding is live

body:
New accounts now finish setup without a call, and day-one activation is up from
48% to 61% in the first week.

[…the rest of the body develops the story: what changed and why, the decisions
behind it, what's still open, and what's next…]
```

## Quick check before sending

- Does every line carry its own context — the point answered without drilling down?
- Could a teammate skim it in 10 seconds and know what's next + what's blocked?
- Is every person an @mention or full name?
- Are enumerable items in bullets?
- (Goal update) Does it lead with the bottom line, does `confidence` match reality, and is the length all signal?
