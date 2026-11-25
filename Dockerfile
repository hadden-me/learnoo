# syntax=docker/dockerfile:1
# Learnoo application image (LRN-010). It contains only the application
# server (cmd/server) with its embedded templates, CSS and KaTeX assets, plus
# the built Studio bundle. The renderer experiment (cmd/renderer) is neither
# built nor shipped. Runs as the distroless non-root user (65532).
#
#   docker build -t learnoo:local .
#
# Base images are pinned by version and digest; update both together.
ARG NODE_IMAGE=node:22.20.0-bookworm-slim@sha256:b21fe589dfbe5cc39365d0544b9be3f1f33f55f3c86c87a76ff65a02f8f5848e
ARG GO_IMAGE=golang:1.25.4-bookworm@sha256:e17419604b6d1f9bc245694425f0ec9b1b53685c80850900a376fb10cb0f70cb
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Browser assets: pinned KaTeX copied for embedding, design CSS, Studio bundle.
FROM ${NODE_IMAGE} AS assets
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY web/studio/package.json web/studio/package-lock.json web/studio/
RUN cd web/studio && npm ci --ignore-scripts --no-audit --no-fund
COPY . .
RUN npm run assets && cd web/studio && npm run build

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=assets /src/internal/rendering/static/vendor internal/rendering/static/vendor
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/learnoo-server ./cmd/server \
 && mkdir -m 0750 /out/media

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/learnoo-server /usr/local/bin/learnoo-server
# The Studio bundle, served under /studio to signed-in staff (LEARNOO_STUDIO_DIR).
COPY --from=assets /src/web/studio/dist /opt/learnoo/studio
# Mount point of the persistent media volume; a new named volume inherits this owner.
COPY --from=build --chown=65532:65532 /out/media /var/lib/learnoo/media
USER 65532:65532
# The image keeps the server's loopback default, so a bare `docker run`
# exposes nothing. The deployment (deploy/compose.production.yaml) sets
# LEARNOO_ADDR=0.0.0.0:8090 and the explicit LEARNOO_ALLOW_NON_LOOPBACK=1
# opt-in, and publishes the port only to the reverse proxy's address.
ENV LEARNOO_MEDIA_DIR=/var/lib/learnoo/media
ENV LEARNOO_STUDIO_DIR=/opt/learnoo/studio
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/learnoo-server", "probe", "-path", "/healthz"]
ENTRYPOINT ["/usr/local/bin/learnoo-server"]
CMD ["serve"]
