---
name: slack-webhook
description: Post a message to Slack through an incoming webhook -- payload shape, Block Kit formatting, delivery checks, and the rules that keep an unattended poster well-behaved. Use when a routine needs to send anything to Slack via $SLACK_WEBHOOK_URL.
---

# Posting to Slack via incoming webhook

The webhook URL arrives as `$SLACK_WEBHOOK_URL`. It both authenticates and
addresses the channel -- never print it, never include it in a message.

## Sending

```bash
curl -sS -o /tmp/slack-resp -w "%{http_code}" -X POST "$SLACK_WEBHOOK_URL" \
  -H "Content-Type: application/json" \
  -d @/tmp/payload.json
```

Treat exactly HTTP 200 with body `ok` as delivered. Anything else --
`no_service`, `invalid_payload`, a 4xx/5xx -- means not delivered; do not
retry more than once in a run.

## Payload shape

Always include a `text` fallback (used by notifications and screen
readers), then `blocks` for structure:

```json
{
  "text": "Agent report: 3 updates, 1 needs a human",
  "blocks": [
    { "type": "header", "text": { "type": "plain_text", "text": "Agent report" } },
    { "type": "section", "text": { "type": "mrkdwn", "text": "*What happened*\n• Shipped the doc updates for the 2.1 release (<https://example.com/pr/42|PR #42>)" } },
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Needs a human*\n• `task-20260725-1` Renew the staging TLS cert" } }
  ]
}
```

Slack mrkdwn is not markdown: links are `<url|text>`, bold is `*text*`,
bullets are literal `•` characters. Keep any single section block under
3000 characters; split long lists across blocks.

## Conduct

- Never use `@channel`, `@here`, or user pings -- an unattended agent
  earns attention with content, not interruptions.
- One message per run, maximum. Batch, don't stream.
- No secrets, tokens, or internal URLs the channel's audience shouldn't
  see; when unsure, name the thing without linking it.
