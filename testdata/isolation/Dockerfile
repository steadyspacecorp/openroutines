# Minimal stand-in for the deployable agent image, used by bin/smoke's
# in-container isolation test: the same identity layout and file capabilities
# as template/Dockerfile (keep the useradd loop in lockstep with it), a
# locally built openroutines binary instead of a release, and a fake opencode.
# The test exists because every production-only gate in the runner is keyed on
# OPENROUTINES_IN_CONTAINER=1, which the host test suite never sets.
FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends git libcap2-bin \
    && rm -rf /var/lib/apt/lists/* \
    && groups=""; \
       for i in $(seq 0 32); do \
         uid=$((20000 + i)); \
         groupadd --gid "${uid}" "attempt-${i}"; \
         useradd --no-create-home --uid "${uid}" --gid "${uid}" --shell /usr/sbin/nologin "attempt-${i}"; \
         groups="${groups},attempt-${i}"; \
       done \
    && useradd -m -u 10001 -G "${groups#,}" agent
COPY opencode /usr/local/bin/opencode
COPY openroutines /usr/local/bin/openroutines
# CAPS is extracted from the scaffolded template Dockerfile by bin/smoke, so
# this image can never hold a broader grant than production does.
ARG CAPS=cap_setuid,cap_setgid,cap_kill
ARG PROFILE=uid
RUN chmod 0755 /usr/local/bin/opencode \
    && mkdir -p /usr/local/lib/openroutines \
    && cp /usr/local/bin/openroutines /usr/local/lib/openroutines/sandbox-exec \
    && chown root:root /usr/local/lib/openroutines/sandbox-exec \
    && chmod 0755 /usr/local/lib/openroutines/sandbox-exec \
    && chown root:agent /usr/local/bin/openroutines \
    && chmod 0750 /usr/local/bin/openroutines \
    && if [ "${PROFILE}" = landlock ]; then \
         test -z "$(getcap /usr/local/bin/openroutines)"; \
       else \
         setcap "${CAPS}=ep" /usr/local/bin/openroutines; \
       fi
COPY --chown=agent agent /agent
RUN chmod 0700 /agent
ENV OPENROUTINES_IN_CONTAINER=1
USER agent
ENV HOME=/home/agent
WORKDIR /agent
