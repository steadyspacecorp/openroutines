# The runtime for this agent.
#
# Privilege moves in one direction: base installs everything as root into system
# paths, and each final stage drops to the unprivileged agent user, which can run
# the binaries but never replace them.
#
#   base    -- OS packages (git, gh, jq, ssh), pinned opencode, the agent user.
#
#   runtime -- base, as the agent user. `openroutines routines run/test` executes
#              the model process in this stage with the run workspace mounted;
#              nothing else from your machine is visible to it.
#
#   agent   -- the deployable image and default build target: base + the pinned
#              openroutines release (the supervisor, its entrypoint) + this repository.
#              A sealed box: no ports, logs on stdout, and two secrets mounted
#              read-only at boot, named by OPENROUTINES_MASTER_KEY_FILE and
#              OPENROUTINES_DEPLOY_KEY_FILE. Neither secret is ever built into the image.

FROM debian:trixie-slim AS base

# Tools every run can rely on: git, gh for GitHub work, jq for reading JSON APIs,
# ssh for git-over-ssh memory pushes, curl for the installs below.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git gh jq openssh-client curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# opencode's installer only targets $HOME, so relocate the binary to the system path.
ARG OPENCODE_VERSION=1.18.3
RUN curl -fsSL https://opencode.ai/install | bash -s -- --version ${OPENCODE_VERSION} --no-modify-path \
    && install -m 0755 -o root -g root /root/.opencode/bin/opencode /usr/local/bin/opencode \
    && rm -rf /root/.opencode

RUN useradd -m -u 10001 agent
ENV OPENROUTINES_IN_CONTAINER=1

FROM base AS runtime
USER agent
ENV HOME=/home/agent

FROM base AS agent
# The framework pin, kept in lockstep with .openroutines-version by `openroutines update`.
ARG OPENROUTINES_VERSION={{OPENROUTINES_VERSION}}
RUN curl -fsSL https://get.openroutines.dev/install.sh | OPENROUTINES_INSTALL_DIR=/usr/local/bin bash

# The same drop runtime makes: sibling stages each end by shedding root.
USER agent
ENV HOME=/home/agent
WORKDIR /agent
ENTRYPOINT ["openroutines", "supervise"]
COPY --chown=agent . /agent
