ARG BASE_RUNTIME
FROM scratch AS archive
ADD runtime.tar.gz /

FROM ${BASE_RUNTIME} AS runtime
RUN rm -rf /opt/java/openjdk
COPY --from=archive --chown=0:0 /jdk-25.0.5+8/ /opt/java/openjdk/
ENV JAVA_VERSION=jdk-25.0.5+8
COPY VerifyRuntime.java /tmp/VerifyRuntime.java
RUN java /tmp/VerifyRuntime.java > /golden-runtime-verification.json && rm /tmp/VerifyRuntime.java

FROM scratch AS evidence
COPY --from=runtime /golden-runtime-verification.json /
