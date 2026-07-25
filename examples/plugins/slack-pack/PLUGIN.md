---
name: slack-pack
description: Post the agent's memory change feed to a Slack channel as a teammate-style update, via an incoming webhook.
credentials:
  slack_webhook_url:
    description: A Slack incoming-webhook URL (https://hooks.slack.com/services/...) -- it both authenticates and addresses the channel, so treat it as a secret
---

# Slack pack

Points an agent's reporting at a Slack channel. The routine is a memory-feed consumer: twice a workday it turns everything the agent recorded since its last report into one short, human update and posts it through an incoming webhook.

## What you get

- **slack-report** -- consumes the memory change feed and posts a digest: what happened, what's now on someone's plate, what changed in the task list. Nothing new since last time means no post -- the channel never gets a "nothing to report" message.
- **slack-webhook** skill -- how to format and send the message: one plain-text fallback, Block Kit sections, no @channel.

## After installing

1. Create an incoming webhook in your Slack workspace (Slack app → Incoming Webhooks → pick the channel).
2. `openroutines credentials set slack_webhook_url`
3. Adjust the schedule, then `openroutines check`, review the diff, commit.

Works alongside other consumers: each keeps its own cursor over the same feed, so adding Slack changes nothing about the routines doing the work -- or about any other destination already reporting.
