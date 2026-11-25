# offerhound — single Go binary + bundled Chromium for the sync engine.
#
# Headless is fully supported out of the box. Headful inside a container
# needs an X server:  docker run ... xvfb-run -a offerhound sync <bank> ...
# `offerhound doctor` (run automatically as the healthcheck) reports
# exactly which of these your container can do.

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/offerhound ./cmd/offerhound

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium ca-certificates fonts-liberation fonts-noto-color-emoji tini xvfb \
 && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 10001 offerhound
# /data is the VOLUME: create it owned, else non-root can't write HOME on
# runs without a volume mount (Debian chromium's crashpad then dies with
# "chrome_crashpad_handler: --database is required")
RUN mkdir -p /data && chown offerhound:offerhound /data
ENV HOME=/data
# containers usually lack unprivileged user namespaces; chromium needs this
ENV OFFERHOUND_BROWSER_FLAGS=--no-sandbox,--disable-dev-shm-usage,--crash-dumps-dir=/tmp/offerhound-crash
COPY --from=build /out/offerhound /usr/local/bin/offerhound

USER offerhound
VOLUME /data
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s \
  CMD ["/usr/local/bin/offerhound","doctor","--probe","http://127.0.0.1:8090/api/users"]
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/offerhound"]
CMD ["serve","--listen","0.0.0.0:8090"]
