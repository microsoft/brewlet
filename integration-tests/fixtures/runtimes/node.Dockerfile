FROM docker.io/library/node@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392
USER 0
COPY prepare-root.sh /tmp/brewlet-fixture-prepare-root.sh
RUN sh /tmp/brewlet-fixture-prepare-root.sh && rm /tmp/brewlet-fixture-prepare-root.sh
ENV PATH="/usr/local/bin:/usr/bin:/bin" HOME="/home/app"
USER 65532:65532
