# insights-api

Answers a fixed catalogue of business questions about the OpenChoreo estate by
composing results from the observer's MCP tools. It queries **no database
directly** — every answer is assembled from federated MCP tool calls, and each
response names the tools it used.

## Why a fixed catalogue and not free text

This service sits behind `business-portal`, which is served publicly to
anonymous visitors. Free-text input routed to an LLM is a materially different
exposure from a button that runs a known query, so v1 offers five pre-defined
questions and refuses any id outside that set. The catalogue *is* the
authorization boundary: there is no path from visitor input to an arbitrary tool
call.

Adding free text later is additive — keep `/api/ask` and put a text-to-question
mapping step in front of it. The MCP layer underneath does not change.

## API

| Route | Purpose |
|---|---|
| `GET /healthz` | Liveness. Never touches the gateway, so a missing gateway does not fail the pod. |
| `GET /api/questions` | The catalogue, as `[{id, label, description}]`. |
| `POST /api/ask` | Body `{"questionId":"<id>"}`. Returns `{answer, data, sources, asOf}`. |
| `GET /api/debug/tools` | Lists the tools the gateway is fronting. The quickest way to tell whether MCP is reachable at all. |

`sources` is the point of the demo, not decoration: it names the MCP tools each
answer was composed from, so a visitor sees the federation working rather than
taking it on trust.

## Environment

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8080` | |
| `MCP_GATEWAY_URL` | *(unset)* | Injected by the `ai-mcp-federation` trait. Never hardcode it. If unset, the service still starts and still serves the catalogue; `/api/ask` returns a clear 503 saying the trait is not attached. |
| `MCP_GATEWAY_PATH` | `/mcp` | Appended only when `MCP_GATEWAY_URL` carries no path of its own. |
| `INSIGHTS_NAMESPACE` | `boutique` | |
| `INSIGHTS_ENVIRONMENT` | `development` | The only populated environment on the showroom. |
| `INSIGHTS_LOOKBACK` | `24h` | Go duration. |

## The questions

1. **cost** — `query_costs` + `query_recommendations`
2. **recent-changes** — `query_dora_deployments`
3. **busiest** — `query_http_metrics`
4. **headroom** — `query_resource_metrics`
5. **cadence** — `query_dora_metrics`

Chosen because these tools have real data on the showroom. `query_alerts` and
`query_incidents` are deliberately unused (the alert evaluator does not fire on
this install), and `query_component_logs` was unusable until its index template
landed. A question whose answer is always "no data" is worse than no question.

**`cadence` states a known gap in its own answer** rather than hiding it: DORA
lead time needs commit-to-deploy provenance, and these workloads are built with
`spec.source: none`, so lead time is structurally absent rather than fast.

## One environment only, and why that is not a shortcut

This component is bound to `development` alone, decided 2026-10-29.

The service takes its query scope from `INSIGHTS_NAMESPACE` and
`INSIGHTS_ENVIRONMENT`. A single `Workload` is shared by every
`ProjectReleaseBinding`, so those are one value for all environments. Bind a
second environment with nothing else and you get an instance reporting
**development's** numbers while sitting in another environment — and
`production-us` is fully running (8/8 pods in the storefront cell, verified
2026-10-29), so that is live wrong data, not an empty page.

**The remedy exists, and it is one field:**
`ReleaseBinding.spec.workloadOverrides.container.env`
(`releasebinding_types.go:189`; `ContainerOverride.Env` at `:142`).
`ReleaseBinding` carries a required `Environment`, so it is per-environment by
construction, and the merge is **override-wins by key** — `mergeEnvConfigs`
(`internal/pipeline/component/context/workload.go:30`) keys the overrides and
replaces any matching base key. A per-environment override of
`INSIGHTS_ENVIRONMENT` does exactly the right thing.

**Why it is still development-only today:** this repository authors
`ProjectReleaseBinding`s only — 25 of them, zero component `ReleaseBinding`s.
Component `ReleaseBinding`s are *derived* by the controllers, so the override
cannot be committed here; it has to be patched onto the derived object, the same
practice as the incident driver's `traitEnvironmentConfigs` patch. The remedy
sits outside the GitOps path, which is **not** the same as the platform being
unable to express it. That distinction matters: the first reading would send
someone off to build a second component or fork the Workload, when the answer
is one field on a derived binding.

**A patch survives reconcile**, checked in source rather than assumed: on an
existing `ReleaseBinding` the component controller mutates only
`Spec.ReleaseName`, and only when it differs
(`internal/controller/component/controller.go:796-806`), carrying everything
else through. Nothing live uses the field yet — 0 of 31 bindings — so it is
unexercised in practice. Worth one real patch-and-watch before relying on it.

**Not the downward API.** The pod does carry `openchoreo.dev/environment` as a
label, but `Workload.spec.container.env.valueFrom` accepts **only
`secretKeyRef`**, so there is no `fieldRef` to read it with. That route is
genuinely closed; the `workloadOverrides` route is not.

The four other binding files are kept under `release-bindings/`, fully
commented out, with this reasoning at the top of each. They are inert: a
directory apply skips them and the repo's `kustomization.yaml` enumerates
resources explicitly, so neither path picks them up.

## Response shapes

Tool responses are flattened generically — `{"items":[...]}`, a bare array, or a
single object all render as a table. The observer's handlers marshal their own
response types, so hardcoding each schema would break this service whenever an
unrelated response type gained a field.

## Running locally

```sh
export PATH=$PATH:/usr/local/go/bin
go build ./...
MCP_GATEWAY_URL=http://localhost:3000 PORT=8080 go run .
curl localhost:8080/api/questions
curl -X POST localhost:8080/api/ask -H 'Content-Type: application/json' -d '{"questionId":"cost"}'
```

No third-party dependencies, by design: the image builds where a module proxy
may not be reachable.
