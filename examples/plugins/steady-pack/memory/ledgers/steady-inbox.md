# steady-inbox ledger

Working state: ids of comments already replied to, so no comment is ever
answered twice. Prune ids once their comment ages out of the 7-day
collection window.

Format, one line per handled comment:

- replied comment_12345 (2026-07-20, on check-in_678)
