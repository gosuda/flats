# Linux container image for the Flats host.
#
# The image packages a release archive from dist/ instead of compiling, so a
# published image runs the same binary as the attested flats_linux_<arch>
# archive of its release. To build one from a checkout:
#
#   FLATS_RELEASE_TARGETS="linux/amd64 linux/arm64" scripts/build-release.sh v0.0.0-dev dist
#   docker buildx build --platform linux/amd64,linux/arm64 -t flats:dev .
#
# (A single-platform `docker build -t flats:dev .` needs only the archive for
# the local architecture.) See docs/container.md for how to run it.

ARG ALPINE_IMAGE=alpine:3.22

# Unpack on the build platform: no emulation, and the archive stays out of the
# final image.
FROM --platform=$BUILDPLATFORM ${ALPINE_IMAGE} AS unpack
ARG TARGETOS
ARG TARGETARCH
COPY dist/checksums.txt dist/flats_${TARGETOS}_${TARGETARCH}.tar.gz /dist/
RUN cd /dist \
	&& grep " flats_${TARGETOS}_${TARGETARCH}.tar.gz\$" checksums.txt | sha256sum -c - \
	&& tar -xzf "flats_${TARGETOS}_${TARGETARCH}.tar.gz" flats

FROM ${ALPINE_IMAGE}
# 65532 is the conventional unprivileged "nonroot" ID. /data is the data
# directory, config.json included (docs/configuration.md).
RUN install -d -o 65532 -g 65532 -m 0700 /data /home/flats
COPY --from=unpack /dist/flats /usr/local/bin/flats
COPY --chmod=0755 scripts/docker-entrypoint.sh /usr/local/bin/flats-entrypoint
ENV FLATS_DATA=/data \
	FLATS_CONFIG=/data/config.json \
	HOME=/home/flats
VOLUME ["/data"]
USER 65532:65532
WORKDIR /home/flats
# `flats status` exits non-zero when the management listener does not answer
# at FLATS_URL (default http://127.0.0.1:7878); set FLATS_URL with --listen.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
	CMD ["flats", "status", "--json"]
STOPSIGNAL SIGTERM
LABEL org.opencontainers.image.title="flats" \
	org.opencontainers.image.description="Open-source alternative to sites/artifacts: deploy sites and small server apps to your own machine" \
	org.opencontainers.image.source="https://github.com/gosuda/flats" \
	org.opencontainers.image.licenses="MIT"
ENTRYPOINT ["flats-entrypoint"]
CMD ["serve"]
