# context-guru-proxy: the LLM-proxy integration and eval-containers gateway.
#
# bifrost is an ordinary published dependency, so the build context is just this
# repo:
#
#   docker build -t context-guru-proxy .
#
# The image is glibc-based because CGO is enabled below. It also carries a shell +
# curl for the eval-containers start/health scripts under /opt/gateway.
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=1 go build \
	-ldflags "-s -w -X github.com/rossoctl/context-guru/internal/buildinfo.Version=${VERSION} -X github.com/rossoctl/context-guru/internal/buildinfo.Commit=${COMMIT}" \
	-o /out/context-guru-proxy ./cmd/context-guru-proxy

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=build /out/context-guru-proxy /opt/gateway/main
COPY deploy/eval-containers/start /opt/gateway/start
COPY deploy/eval-containers/health /opt/gateway/health
RUN chmod +x /opt/gateway/start /opt/gateway/health
EXPOSE 4000
# Default entrypoint is the eval-containers gateway wrapper; override with
# `/opt/gateway/main` to run the proxy directly with flags.
ENTRYPOINT ["/opt/gateway/start"]
