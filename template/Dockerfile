# Runtime for this openroutines agent.
#
# Two stages:
#   runtime -- debian + git + pinned opencode. Local `openroutines routines
#              run/test` executes the model process in this stage with the
#              run workspace mounted; nothing else from your machine is
#              visible to it.
#   agent   -- the deployable image: runtime + this repository, with the
#              supervisor as entrypoint. A sealed box: no ports, two env-var
#              secrets (OPENROUTINES_MASTER_KEY, OPENROUTINES_DEPLOY_KEY),
#              logs on stdout.

ARG OPENCODE_VERSION=1.18.3

FROM debian:bookworm-slim AS runtime
ARG OPENCODE_VERSION

RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates curl bash \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 agent

USER agent
ENV HOME=/home/agent
RUN curl -fsSL https://opencode.ai/install | bash -s -- --version ${OPENCODE_VERSION} --no-modify-path
ENV PATH="/home/agent/.opencode/bin:${PATH}"
ENV OPENROUTINES_IN_CONTAINER=1

FROM runtime AS agent
# TODO(release): install the openroutines binary pinned by
# .openroutines-version, verified by checksum, once public releases exist.
# Until then, deploying requires building the binary into this stage yourself.
WORKDIR /agent
COPY --chown=agent . /agent
ENTRYPOINT ["openroutines", "supervise"]
