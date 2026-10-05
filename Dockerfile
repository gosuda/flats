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

ARG UNPACK_IMAGE=alpine:3.22
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot

# Unpack on the build platform: no emulation, and the archive stays out of the
# final image.
FROM --platform=$BUILDPLATFORM ${UNPACK_IMAGE} AS unpack
ARG TARGETOS
ARG TARGETARCH
COPY dist/checksums.txt dist/flats_${TARGETOS}_${TARGETARCH}.tar.gz /dist/
RUN cd /dist \
	&& grep " flats_${TARGETOS}_${TARGETARCH}.tar.gz\$" checksums.txt | sha256sum -c - \
	&& tar -xzf "flats_${TARGETOS}_${TARGETARCH}.tar.gz" flats \
	&& mkdir -p -m 0700 /out/data

# The flats binary is static (CGO-free). The distroless base adds CA
# certificates, tzdata and the unprivileged user nonroot (65532), and has no
# shell or package manager.
FROM ${BASE_IMAGE}
COPY --from=unpack --chown=0:0 /dist/flats /usr/local/bin/flats
# /data is the data directory, config.json included (docs/configuration.md).
# A new named volume takes this directory's owner and mode.
COPY --from=unpack --chown=65532:65532 --chmod=0700 /out/data /data
# With FLATS_CONFIG set, `flats serve` runs from config.json, and creates it
# with an empty database when /data holds no Flats data yet.
ENV FLATS_DATA=/data \
	FLATS_CONFIG=/data/config.json
VOLUME ["/data"]
USER 65532:65532
# `flats status` exits non-zero when the management listener does not answer
# at FLATS_URL (default http://127.0.0.1:7878); set FLATS_URL with --listen.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
	CMD ["flats", "status", "--json"]
STOPSIGNAL SIGTERM
LABEL org.opencontainers.image.title="flats" \
	org.opencontainers.image.description="Open-source alternative to sites/artifacts: deploy sites and small server apps to your own machine" \
	org.opencontainers.image.source="https://github.com/gosuda/flats" \
	org.opencontainers.image.licenses="MIT"
ENTRYPOINT ["flats"]
CMD ["serve"]
