ARG RUNTIME_BASE=docker.io/library/eclipse-temurin@sha256:000fd431958bc81a24abe1e8e5f0f0fd3ae365a594bd50aadb20696805f9408c
FROM ${RUNTIME_BASE}
USER 0
COPY prepare-root.sh /tmp/brewlet-fixture-prepare-root.sh
RUN sh /tmp/brewlet-fixture-prepare-root.sh && rm /tmp/brewlet-fixture-prepare-root.sh
ENV PATH="/opt/java/openjdk/bin:/usr/local/bin:/usr/bin:/bin" HOME="/home/app"
USER 65532:65532
