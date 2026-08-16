# Run appendix

## One run, end to end

```mermaid
sequenceDiagram
    participant S as Supervisor
    participant R as Routine
    participant K as Knowledge worktree
    participant O as Origin

    S->>K: commit and push pending run
    S->>K: reserve and push attempt
    S->>R: run in isolated workspace
    R->>R: read and update staged knowledge
    R-->>S: exit or timeout
    S->>K: validate, import, and record outcome
    S->>O: push knowledge; retry failed push later
```
