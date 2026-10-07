FROM debian:13-slim@sha256:d7e12182ce18b85b93007c1dedf31f2d29e01ccf3182cc4017c709b6259bc132
RUN apt-get update && apt-get install -y --no-install-recommends \
    falkon firefox-esr xvfb ffmpeg openbox xterm x11-xkb-utils xauth \
    fonts-dejavu-core fonts-liberation ca-certificates procps \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 app && useradd -u 1000 -g app -m -s /bin/bash app
COPY bin/session-worker /usr/local/bin/session-worker
COPY deploy/worker/openbox.xml deploy/worker/welcome.html /opt/mvp1/
ENV HOME=/home/app DISPLAY=:99
USER 0:0
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/session-worker"]
