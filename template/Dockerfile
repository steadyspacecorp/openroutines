# Runtime image for this openroutines agent.
#
# The container is a sealed box: no ports, two env-var secrets
# (OPENROUTINES_MASTER_KEY, OPENROUTINES_DEPLOY_KEY), logs on stdout.
# See https://openroutines.dev and DESIGN.md in the openroutines repo.

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# Install the pinned openroutines release (checksummed) and opencode.
# TODO(release): replace with checksum-verified downloads of the version in
# .openroutines-version once tagged releases exist.
COPY .openroutines-version /tmp/.openroutines-version
RUN echo "openroutines $(cat /tmp/.openroutines-version) -- release install pending" \
    && curl -fsSL https://opencode.ai/install | bash

# The agent repository (main branch) is the image's payload.
WORKDIR /agent
COPY . /agent

# No ports are exposed -- by design.
ENTRYPOINT ["openroutines", "supervise"]
