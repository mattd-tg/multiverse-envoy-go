# Go build environment for compiling the proxy plugin
FROM golang:1.24.5-bookworm@sha256:69adc37c19ac6ef724b561b0dc675b27d8c719dfe848db7dd1092a7c9ac24bc6 AS golang-base

# Envoy runtime with Go plugin support
FROM envoyproxy/envoy:contrib-dev AS envoy-base
ARG ENVOY_CONFIG=envoy.yaml
ENV ENVOY_CONFIG="$ENVOY_CONFIG"
ENV DEBIAN_FRONTEND=noninteractive
RUN echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-retries
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    apt-get -qq update \
    && apt-get -qq install --no-install-recommends -y curl
COPY --chmod=777 "$ENVOY_CONFIG" /etc/envoy.yaml
CMD ["/usr/local/bin/envoy", "-c", "/etc/envoy.yaml"]

# Final Envoy image with the compiled Go plugin
FROM envoy-base AS envoy-go
ENV GODEBUG=cgocheck=0
COPY --chmod=777 ./lib/proxy.so /lib/proxy.so