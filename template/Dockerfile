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
    && apt-get install -y --no-install-recommends git gh jq openssh-client curl ca-certificates libcap2-bin \
    && rm -rf /var/lib/apt/lists/* \
    && for i in $(seq 0 31); do \
         uid=$((20000 + i)); \
         groupadd --gid "${uid}" "attempt-${i}"; \
         useradd --no-create-home --uid "${uid}" --gid "${uid}" --shell /usr/sbin/nologin "attempt-${i}"; \
       done \
    && find / -xdev -type f -perm /6000 -exec chmod a-s {} + \
    && useradd -m -u 10001 agent

# opencode's installer only targets $HOME, so relocate the binary to the system path.
ARG OPENCODE_VERSION=1.18.3
RUN curl -fsSL https://opencode.ai/install | bash -s -- --version ${OPENCODE_VERSION} --no-modify-path \
    && install -m 0755 -o root -g root /root/.opencode/bin/opencode /usr/local/bin/opencode \
    && rm -rf /root/.opencode

ENV OPENROUTINES_IN_CONTAINER=1

FROM base AS runtime
USER agent
ENV HOME=/home/agent

FROM base AS agent
# The framework pin, kept in lockstep with .openroutines-version by `openroutines update`.
ARG OPENROUTINES_VERSION={{OPENROUTINES_VERSION}}
RUN curl -fsSL https://get.openroutines.dev/install.sh | OPENROUTINES_INSTALL_DIR=/usr/local/bin bash
RUN mkdir -p /usr/local/lib/openroutines \
    && cp /usr/local/bin/openroutines /usr/local/lib/openroutines/sandbox-exec \
    && chown root:root /usr/local/lib/openroutines/sandbox-exec \
    && chmod 0755 /usr/local/lib/openroutines/sandbox-exec
RUN chown root:agent /usr/local/bin/openroutines \
    && chmod 0750 /usr/local/bin/openroutines \
    && setcap cap_setuid,cap_setgid=ep /usr/local/bin/openroutines \
    && getcap /usr/local/bin/openroutines | grep -Eq 'cap_setgid,cap_setuid=ep|cap_setuid,cap_setgid=ep'

COPY --chown=agent . /agent
RUN chmod 0700 /agent \
    && find /agent -type d -exec chmod 0700 {} + \
    && find /agent -type f -perm /0111 -exec chmod 0700 {} + \
    && find /agent -type f ! -perm /0111 -exec chmod 0600 {} +

# The same drop runtime makes: sibling stages each end by shedding root.
USER agent
ENV HOME=/home/agent
WORKDIR /agent
ENTRYPOINT ["openroutines", "supervise"]
