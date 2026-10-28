# One Dockerfile for both programs: --build-arg CMD=agent (default) or CMD=dash.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH CMD=agent VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/app ./cmd/$CMD

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/app /usr/local/bin/app
# The label GHCR reads to link the package to its repository.
LABEL org.opencontainers.image.source="https://github.com/mytview/ytdl-dash"
USER 0
ENTRYPOINT ["/usr/local/bin/app"]
