# business-portal — read-only insights UI.
#
# Deliberately plain. Do not add cross-build arguments or per-arch flags to the
# FROM lines here: the in-cluster build pipeline cannot supply those build args,
# and adding them is what broke every image build in this estate on 2026-10-28.
# README.md names the exact flags to keep out, and a plain grep of this file is
# expected to come back empty for them.
#
# No module downloads either: this service uses the Go standard library only,
# so GOPROXY=off below is a guarantee rather than a restriction. If a build
# ever fails here with a proxy error, a third-party import was added.

FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod ./
COPY *.go ./

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOPROXY=off

RUN go build -trimpath -ldflags="-s -w" -o /out/business-portal .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/business-portal /business-portal

ENV PORT=8080
EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/business-portal"]
