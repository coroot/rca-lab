FROM maven:3.9-eclipse-temurin-21 AS build
WORKDIR /app
COPY pom.xml .
RUN mvn dependency:go-offline -B
COPY src ./src
COPY variants ./variants
ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./src/; fi
RUN mvn package -DskipTests -B

# glibc-based JRE (not -alpine/musl): the Kafka client's snappy-java unpacks a
# glibc-linked native lib (libsnappyjava.so) to decompress snappy-compressed
# record batches. On Alpine/musl it fails to load (UnsatisfiedLinkError: missing
# ld-linux-x86-64.so.2), which surfaces as a fatal, unskippable consumer-fetch
# exception that Spring Kafka retries forever — pegging CPU and flooding logs.
FROM eclipse-temurin:21-jre
WORKDIR /app

# OpenTelemetry Java agent, pinned to v2.30.0
# sha256: 9d6bc2ad8dd8fb7f730984988e57b8ac0a82d81c7b3b8ae795378718733a509d
ADD --chmod=644 --checksum=sha256:9d6bc2ad8dd8fb7f730984988e57b8ac0a82d81c7b3b8ae795378718733a509d \
    https://github.com/open-telemetry/opentelemetry-java-instrumentation/releases/download/v2.30.0/opentelemetry-javaagent.jar \
    /otel/opentelemetry-javaagent.jar
ENV JAVA_TOOL_OPTIONS=-javaagent:/otel/opentelemetry-javaagent.jar

COPY --from=build /app/target/*.jar app.jar
EXPOSE 8080
ENTRYPOINT ["java", "-XX:+UseG1GC", "-XX:MaxGCPauseMillis=100", "-XX:+PreserveFramePointer", "-Xmx1g", "-Xms512m", "-jar", "app.jar"]
