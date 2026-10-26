ARG RUNTIME_BASE=docker.io/library/php@sha256:37c8c64177612b72e7f9d2c00481739c166a3708bea27c1cfd27fea1291d82d1
FROM ${RUNTIME_BASE}
USER 0
COPY prepare-root.sh /tmp/brewlet-fixture-prepare-root.sh
RUN sh /tmp/brewlet-fixture-prepare-root.sh && rm /tmp/brewlet-fixture-prepare-root.sh
ENV PATH="/usr/local/bin:/usr/bin:/bin" HOME="/home/app"
USER 65532:65532
