# syntax=docker/dockerfile:1

ARG GO_VERSION=1.25
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

ARG TARGETARCH

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,target=. \
    mkdir /app && \
    CGO_ENABLED=0 GOARCH=$TARGETARCH go build -o /app/zqp .

FROM alpine:latest AS final

RUN --mount=type=cache,target=/var/cache/apk \
    apk --update --no-cache add \
    ca-certificates \
    tzdata \
    && \
    update-ca-certificates

ARG UID=10001
ARG CONFIG=config/cassiopeia.yaml
RUN adduser \
    --disabled-password \
    --gecos "" \
    --home "/app" \
    --shell "/sbin/nologin" \
    --uid "${UID}" \
    appuser
USER appuser

COPY --from=build --chown=appuser:appuser /app /app
COPY --chown=appuser:appuser ${CONFIG} /app/config.yaml
WORKDIR /app
# EXPOSE 8080

ENTRYPOINT [ "/bin/sh","-c" ]
CMD [ "/app/zqp" ]
