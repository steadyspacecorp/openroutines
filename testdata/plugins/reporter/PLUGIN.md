---
name: reporter
description: Synthetic acceptance fixture -- a knowledge-feed consumer with a bundled skill, a raw credential, and a declared MCP server.
credentials:
  reporter_token:
    description: Fake token for the acceptance test; never a real value
mcp:
  reporter_feed:
    description: Fake MCP server the digest routine reads
    url: https://mcp.example.invalid/feed
    credential: reporter_token
---

# reporter

A synthetic fixture for the acceptance suite, not a real plugin: one consumer routine, one skill, one raw credential, one declared MCP server. It exists so the tests can exercise the install contract without depending on any real plugin. Never install it in a real agent.
