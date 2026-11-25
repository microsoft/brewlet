#!/bin/sh
set -eu

if [ "$#" -ne 0 ]; then
  echo "The platform Tomcat launcher does not accept arguments." >&2
  exit 64
fi
if [ -n "${JAVA_TOOL_OPTIONS-}" ] || [ -n "${JDK_JAVA_OPTIONS-}" ] \
    || [ -n "${_JAVA_OPTIONS-}" ] || [ -n "${JAVA_OPTS-}" ] \
    || [ -n "${CATALINA_OPTS-}" ]; then
  echo "Application-supplied JVM options are not permitted by the platform Tomcat launcher." >&2
  exit 64
fi

exec /opt/java/openjdk/bin/java \
  -Dcatalina.home=/usr/local/tomcat \
  -Dcatalina.base=/opt/brewlet/tomcat \
  -Djava.io.tmpdir=/tmp \
  -Djava.util.logging.config.file=/opt/brewlet/tomcat/conf/logging.properties \
  -Djava.util.logging.manager=org.apache.juli.ClassLoaderLogManager \
  --add-opens=java.base/java.lang=ALL-UNNAMED \
  --add-opens=java.base/java.io=ALL-UNNAMED \
  --add-opens=java.base/java.util=ALL-UNNAMED \
  --add-opens=java.base/java.util.concurrent=ALL-UNNAMED \
  --add-opens=java.rmi/sun.rmi.transport=ALL-UNNAMED \
  --enable-native-access=ALL-UNNAMED \
  -classpath /usr/local/tomcat/bin/bootstrap.jar:/usr/local/tomcat/bin/tomcat-juli.jar \
  org.apache.catalina.startup.Bootstrap start
