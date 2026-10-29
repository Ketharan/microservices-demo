# business-portal

A tiny read-only web UI for the OpenChoreo insights demo. It lists a fixed catalogue of
business questions, lets a visitor click one, and renders the answer returned by the
`insights-api` service — including the MCP tool names that composed it.

Showing those tool names is the point of the demo: the "Answered using" panel on each answer
is how a visitor sees the MCP federation working rather than taking it on faith.

Go standard library only — `net/http`, `html/template`, `encoding/json`. No third-party
modules, no `go.sum`, nothing to fetch at build time.

## No free-text input in v1

There is no search box, and that is deliberate. This page is served publicly to anonymous
visitors. A button that runs a known, pre-defined query is a different exposure from a text
field routed to an LLM, so v1 offers only the questions `insights-api` publishes in its
catalogue. The portal also refuses a question id that is not in that catalogue before it ever
reaches the backend.

Adding free text later is additive: `/api/ask` stays as it is and an LLM step maps text to a
question id. The MCP layer underneath does not change.

## Environment variables

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `PORT` | no | `8080` | Port the HTTP server listens on. |
| `INSIGHTS_API_ADDR` | yes, in practice | — | Address of `insights-api`. Accepts `host:port` (`insights-api:8080`) or a full URL (`http://insights-api:8080`); a value with no scheme gets `http://` prepended. Injected by the platform from the component's `dependencies.endpoints` binding — never hardcoded here. |

If `INSIGHTS_API_ADDR` is unset or unusable, the UI still starts and `/` still returns 200
with a banner saying the backend is not configured.

## Routes

| Route | What it does |
|---|---|
| `GET /` | Fetches the catalogue from the backend's `/api/questions` at request time and renders one card per question. |
| `POST /ask` (form, field `q`) | Calls the backend's `POST /api/ask` and renders the answer prose, the `sources` list, and `data` as a table. |
| `GET /ask?q=<id>` | Same, for linking and for `curl`. |
| `GET /healthz` | `200 ok`. Does not touch the backend, so it stays a signal about this process only. |

Backend responses are treated defensively: `data` may be absent, `null`, or have no rows, and
the page then says "no rows returned". A backend that is unreachable or returns
`{"error": "..."}` produces a readable error banner with the rest of the page intact, served
as `200` so that an intermediate proxy does not swap the body for its own error page.

## Run it locally

```sh
export PATH=$PATH:/usr/local/go/bin   # go is not on PATH in this workspace

go vet ./... && go build ./...

INSIGHTS_API_ADDR=insights-api:8080 PORT=8080 go run .
# then: http://localhost:8080/
```

With no backend to hand, any server that honours the contract will do:

```sh
curl -s localhost:8080/healthz
curl -s localhost:8080/ | head -40
curl -s 'localhost:8080/ask?q=estate-cost'
```

## Container

```sh
docker build -t business-portal:dev .
docker run --rm -p 8080:8080 -e INSIGHTS_API_ADDR=http://host.docker.internal:9090 business-portal:dev
```

Multi-stage: `golang:1.23-alpine` to build a static `CGO_ENABLED=0` binary, then
`gcr.io/distroless/static-debian12:nonroot` to run it as a non-root user on port 8080.

The Dockerfile contains **no** `--platform` flag and **no** `$BUILDPLATFORM` /
`$TARGETPLATFORM` arguments. The build pipeline in this estate cannot supply those build args,
and using them is what broke all 11 image builds here on 2026-10-28. Do not add them.

## Backend contract this UI is built to

```
GET  /healthz        -> 200
GET  /api/questions  -> [{"id":"...","label":"...","description":"..."}]
POST /api/ask  {"questionId":"<id>"}
     -> {"answer":"<prose>",
         "data":{"columns":["..."],"rows":[["..",".."]]},   // may be null or absent
         "sources":["query_costs","query_recommendations"],
         "asOf":"2026-10-28T18:00:00Z"}
     non-200 -> {"error":"<message>"}
```

Every outbound call has a 10s timeout, and response bodies are read through a 2 MiB limit.
