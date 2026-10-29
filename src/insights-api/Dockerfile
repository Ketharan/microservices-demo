# Deliberately NO --platform, $BUILDPLATFORM or $TARGETPLATFORM anywhere in this
# file. The in-cluster buildah pipeline does not supply those build args, and a
# Dockerfile that references them fails the build with an empty-value error --
# that single line broke all 11 boutique builds on 2026-10-28.
FROM golang:1.23-alpine AS build
WORKDIR /src

# Only go.mod is copied before the source because this service has no
# third-party dependencies: there is no go.sum, and nothing is fetched at build
# time. That is a deliberate constraint, not an omission -- the build runs where
# a module proxy may not be reachable.
COPY go.mod ./
COPY *.go ./

# Static binary: the runtime stage has no libc.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/insights-api .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/insights-api /insights-api
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/insights-api"]
