FROM mcr.microsoft.com/dotnet/runtime@sha256:939555726cae4c916d212323ac3c0792996dcac2614f00e82cc40fcf059e1af3
USER 0
COPY prepare-root.sh /tmp/brewlet-fixture-prepare-root.sh
RUN sh /tmp/brewlet-fixture-prepare-root.sh && rm /tmp/brewlet-fixture-prepare-root.sh
ENV PATH="/usr/local/bin:/usr/bin:/bin" HOME="/home/app" APP_UID="65532"
USER 65532:65532
