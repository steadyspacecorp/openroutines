# doc-watch ledger

Working state: one watermark per watched repo -- the last default-branch
commit this routine accounted for. Advance a watermark only after the
events covering it are recorded.

Format, one line per repo:

- acme/docs: 4f2a91c
- acme/runbooks: baseline pending (first run records the head)
