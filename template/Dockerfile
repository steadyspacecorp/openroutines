# Runtime for this openroutines agent.
#
# Two stages:
#   runtime -- debian + git/gh/jq + pinned opencode. Local `openroutines routines
#              run/test` executes the model process in this stage with the
#              run workspace mounted; nothing else from your machine is
#              visible to it. Self-contained: builds without registry access.
#   agent   -- the deployable image: the openroutines base image (supervisor
#              entrypoint included) plus this repository. A sealed box: no
#              ports, two secrets mounted read-only at run time and named by
#              OPENROUTINES_MASTER_KEY_FILE / OPENROUTINES_DEPLOY_KEY_FILE,
#              logs on stdout. Neither secret is ever built into the image.

ARG OPENCODE_VERSION=1.18.3

FROM debian:bookworm-slim AS runtime
ARG OPENCODE_VERSION

RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates curl bash jq \
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 agent

USER agent
ENV HOME=/home/agent
RUN curl -fsSL https://opencode.ai/install | bash -s -- --version ${OPENCODE_VERSION} --no-modify-path
ENV PATH="/home/agent/.opencode/bin:${PATH}"
ENV OPENROUTINES_IN_CONTAINER=1

FROM ghcr.io/steadyspacecorp/openroutines:{{OPENROUTINES_VERSION}} AS agent
COPY --chown=agent . /agent
