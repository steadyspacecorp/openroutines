# Minimal stand-in for the deployable agent image, used by the acceptance suite's
# in-container isolation tests: the bwrap template/Dockerfile's own bubblewrap
# stage builds (the suite builds that stage first and passes it in, so the two
# cannot drift), a locally built openroutines binary instead of a
# release, and a fake opencode. procps is the test's own, for the
# escaped-descendant check. The test exists because every production-only gate
# in the runner is keyed on OPENROUTINES_IN_CONTAINER=1, which the host test
# suite never sets.
ARG BUBBLEWRAP_IMAGE
FROM ${BUBBLEWRAP_IMAGE} AS bubblewrap

FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends git openssh-client libcap2 procps \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 agent
COPY --from=bubblewrap /tmp/build/bwrap /usr/local/bin/bwrap

COPY opencode /usr/local/bin/opencode
COPY ssh /usr/local/bin/ssh
COPY openroutines /usr/local/bin/openroutines
RUN chmod 0755 /usr/local/bin/opencode /usr/local/bin/openroutines /usr/local/bin/ssh
COPY --chown=agent agent /agent
ENV OPENROUTINES_IN_CONTAINER=1
USER agent
ENV HOME=/home/agent
WORKDIR /agent
