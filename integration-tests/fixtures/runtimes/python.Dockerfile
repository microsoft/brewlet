FROM docker.io/library/python@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258
USER 0
COPY prepare-root.sh /tmp/brewlet-fixture-prepare-root.sh
RUN sh /tmp/brewlet-fixture-prepare-root.sh && rm /tmp/brewlet-fixture-prepare-root.sh
ENV PATH="/usr/local/bin:/usr/bin:/bin" HOME="/home/app"
USER 65532:65532
