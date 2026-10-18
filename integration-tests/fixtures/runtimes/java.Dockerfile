ARG RUNTIME_BASE=docker.io/library/eclipse-temurin@sha256:7b5c88eb4182a92aab3a4b10550061a6e18639bf176e7ebca21f866b19f853c1
FROM ${RUNTIME_BASE}
USER 0
COPY prepare-root.sh /tmp/brewlet-fixture-prepare-root.sh
RUN sh /tmp/brewlet-fixture-prepare-root.sh && rm /tmp/brewlet-fixture-prepare-root.sh
ENV PATH="/opt/java/openjdk/bin:/usr/local/bin:/usr/bin:/bin" HOME="/home/app"
USER 65532:65532
