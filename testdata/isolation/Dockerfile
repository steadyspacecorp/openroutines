# Minimal stand-in for the deployable agent image, used by bin/smoke's
# in-container isolation test: the same sandbox dependencies as
# template/Dockerfile (keep them in lockstep, or the smoke test stops
# resembling what deploys), a locally built openroutines binary instead of a
# release, and a fake opencode. procps is the test's own, for the
# escaped-descendant check. The test exists because every production-only gate
# in the runner is keyed on OPENROUTINES_IN_CONTAINER=1, which the host test
# suite never sets.
FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends git bubblewrap procps \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 agent

COPY opencode /usr/local/bin/opencode
COPY openroutines /usr/local/bin/openroutines
RUN chmod 0755 /usr/local/bin/opencode /usr/local/bin/openroutines
COPY --chown=agent agent /agent
ENV OPENROUTINES_IN_CONTAINER=1
USER agent
ENV HOME=/home/agent
WORKDIR /agent
