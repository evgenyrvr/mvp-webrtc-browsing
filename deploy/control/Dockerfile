# Binaries are built by deploy/build-images.sh; no compiler is required on nodes.
FROM debian:13-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY bin/gateway bin/runner /usr/local/bin/
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/gateway"]
