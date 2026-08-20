# The runtime for this agent.
#
# Privilege moves in one direction: base installs everything as root into system
# paths, and each final stage drops to the unprivileged agent user, which can run
# the binaries but never replace them.
#
#   base    -- OS packages and common tools, pinned opencode, the agent user.
#
#   runtime -- base, as the agent user. `openroutines routines run/test` executes
#              the model process in this stage with the run workspace mounted;
#              nothing else from your machine is visible to it.
#
#   agent   -- the deployable image and default build target: base + the pinned
#              openroutines release (the supervisor, its entrypoint) + this repository.
#              A sealed box: no ports, logs on stdout, and two secrets supplied
#              at boot through environment values or mounted at the conventional
#              /agent/master.key and /agent/deploy.key paths. Neither secret is
#              ever built into the image.

FROM debian:trixie-slim AS base

# Tools every run can rely on: git, gh for GitHub work, jq for reading JSON APIs,
# rg for searching, unzip, Python, Node.js, ssh for git-over-ssh knowledge pushes,
# and curl for the installs below.
# bubblewrap is the preferred run sandbox: the supervisor wraps every model
# process in its own mount, pid, ipc, uts and user namespaces, so one run
# cannot see, signal, or name anything belonging to another. It is the only
# sandbox that needs installing -- where the host will not permit those
# namespaces the supervisor falls back to a Landlock domain, which needs
# nothing but the kernel.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git gh jq ripgrep unzip python3 nodejs openssh-client curl ca-certificates bubblewrap \
    && rm -rf /var/lib/apt/lists/* \
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
# The framework pin, kept in lockstep with .openroutines/version by `openroutines update`.
ARG OPENROUTINES_VERSION={{OPENROUTINES_VERSION}}
RUN curl -fsSL https://get.openroutines.dev/install.sh | OPENROUTINES_INSTALL_DIR=/usr/local/bin bash
COPY --chown=agent . /agent

# The same drop runtime makes: sibling stages each end by shedding root.
USER agent
ENV HOME=/home/agent
WORKDIR /agent
ENTRYPOINT ["openroutines", "supervise"]
