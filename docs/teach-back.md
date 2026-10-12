# Teach-back notes: building and onboarding a demo with the buildkit

Internal notes for SAs. This repo is a worked example of using the Temporal demo buildkit (`~/Source/temporal-toolkit-demo-buildkit`) to plan and build a customer-facing demo, then onboard it to [tmprl-demo.cloud](https://github.com/temporal-sa/tmprl-demo-cloud-registry).

## 1. Start the build from the kit

The kit is reference material for an agent (or a person), not a framework. The whole kickoff is one prompt:

> Read `~/Source/temporal-toolkit-demo-buildkit/README.md`. Use the guides to discuss suitable options with me as we plan the demo, then build around the choices we make together.

The agent asks a few focused questions and doesn't pick the demo's major direction on its own. For this demo it asked three:

| Question | Options offered | Chosen |
|---|---|---|
| Story | Order checkout (saga + crash), **expense approval**, travel booking | Expense approval |
| Look | Branded shop + dark backstage, **all-dark Temporal theme** | All-dark theme (`assets/css/temporal-theme.css`) |
| SDK | **Go**, Python, TypeScript | Go |

Everything else (layout, session isolation, task queue, health check, packaging) followed the guides.

## 2. The two non-optional baselines

### The Temporal code is the demo (kit guide 07)

Customers will open `expense/workflow.go`, so it reads top to bottom as the business process and uses Temporal features instead of hand-rolled ones:

| Need | Native feature used |
|---|---|
| Wait for a person, escalate, expire | `workflow.AwaitWithTimeout` (durable timers) |
| Deliver the decision | A structured Signal (who, approved, comment, when) with first-valid-decision-wins |
| Survive a payment outage | Retry Policy with backoff; Workflow ID as the idempotency key |
| Report progress | A `status` Query, plus Event History for the UI |
| Failure injection | Lives in the Activity (`DemoOptions`), not the Workflow |

The code panel embeds the running `workflow.go` (`go:embed`), so the excerpt on screen can't drift from the deployed code. Worker logs are logfmt, with SDK retry errors reworded as `WARN activity attempt failed; Temporal will retry it`.

### Catalog-ready from day one (kit guide 12)

Guide 12's ten planning answers for this demo:

| # | Item | This demo |
|---|---|---|
| 1 | Session boundary | 128-bit random `expense_session` cookie per browser (HttpOnly, host-only) |
| 2 | What it scopes | Workflow IDs are `<taskQueue>-<session>-<expenseId>`. Every read and Signal rebuilds the ID from the caller's session, and lists filter on `WorkflowId STARTS_WITH`. Another session's IDs return 404. |
| 3 | Task-queue contract | `TEMPORAL_TASK_QUEUE`, base `expense-approval`. The web app starts work on it and Activities inherit it. |
| 4 | Cross-workspace isolation | The task queue is in the Workflow ID and in the list query (`TaskQueue = …`) |
| 5 | Builds / components | One Dockerfile; `web` (`temporalAccess: true`) and `worker` (`worker: true`, `command: ["/app/worker"]`) |
| 6 | Routes | One ingress; `web` serves `servicePort: 80` for UI and `/api` |
| 7 | Temporal config | `envconfig` with `DisableFile: true`; the platform injects address, namespace, API key, and TLS |
| 8 | Secrets / external state | None. All durable state lives in Temporal. |
| 9 | Smoke check | `GET /healthz` calls `GetSystemInfo` and changes nothing |
| 10 | Known gaps | None found against the current schema |

## 3. Onboarding to the catalog

1. **Move the source** to `github.com/temporal-sa/temporal-demo-expense-approval`. The registry only builds from `temporal-sa`, `temporal-community`, or `temporalio`.
2. **Copy** `deploy/expense-approval.yaml` to `projects/demo/expense-approval.yaml` in `tmprl-demo-cloud-registry`. That one file is the entire onboarding.
3. **Validate** from the registry repo:
   ```sh
   python3 -m pip install -r requirements-dev.txt
   python3 scripts/validate_projects.py projects/demo/expense-approval.yaml
   ```
4. **Open a PR.** On merge, Flux applies the CR and the registry operator derives the rest: the Kubernetes namespace `tmprl-dem-cld-expense-approval`, a Temporal Cloud namespace and API key, image builds, Services, HTTPS ingress at `expense-approval.tmprl-demo.cloud`, and the smoke check before promotion.
5. **Catalog metadata** (`spec.registry`) gives the card's title, summary, tags, SDK, and runbook link. `crashable` turns on per-user workspaces.
6. Recordings and decks go in the demo's folder under Demo Catalog Material (kit guide 13), linked from `spec.registry.links`.

What a **crashable workspace** does: it clones the demo into its own namespace and `crash-<id>.tmprl-demo.cloud` host, sets `TEMPORAL_TASK_QUEUE=expense-approval-<id>` on every component, and gives the user buttons to delete or scale the worker pods. The web pod keeps serving.

## 4. What was verified

- 7 Workflow tests (`make test`): auto-approve, policy reject, approve, reject, escalation rules, expiry, payment retries.
- Locally and in containers: crash the worker → approve → UI shows "waiting for a worker" → replacement worker finishes the request.
- Two browser sessions can't list, read, or signal each other's expenses. A second deployment on another task queue can't see the first, even with the same cookie.
- The image builds from a clean copy and runs with service-DNS Temporal and a non-default task queue.
- Layout checked at 1920×1080, 1366×768, and a 390px phone.
- The `DemoProject` draft passes the registry's `validate_projects.py`.

## 5. Things worth knowing (learned while building)

- **Signals vs Updates when workers can vanish:** a Signal is accepted with no worker running. An Update needs a worker to accept it, so the approver's click would hang during a crash.
- **Build the UI from history, not Queries:** a Query also needs a worker. Event History and `DescribeWorkflowExecution` are served by Temporal alone.
- **Sticky queues:** after a worker dies, its next workflow task waits about 5 seconds on the dead worker's sticky queue before it's rescheduled. Measure "waiting for a worker" from the first unprocessed event, not the task's scheduled time.
- **`AwaitWithTimeout` doesn't record `TimerCanceled`** in the Go SDK when the condition wins. The timer simply goes unused, so a UI shouldn't wait for that event.
- **`ENTRYPOINT` vs `command`:** with `ENTRYPOINT ["/app/web"]`, `docker run image /app/worker` silently runs the web app again. Use `CMD`, so Docker and the registry's `command:` both select the process.
