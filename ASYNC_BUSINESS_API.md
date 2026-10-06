# Business keys and private-R2 asynchronous media

For business developers, see the [Chinese integration guide](BUSINESS_INTEGRATION.zh-CN.md) for credential setup, request/response examples, backend polling, authenticated downloads and usage reconciliation.

## Scope

This additive feature is based on merged `03c85e8`, on branch `codex/newapi-async-business`. Existing synchronous/SSE routes, provider adapters, provider retry and pricing remain authoritative. No Dianmo source, task retry, fallback, database or deployment is changed. Configure New API as an ordinary Dianmo channel separately; that does not make Dianmo requests asynchronous or remove its long-request proxy timeout risk.

The server remains Go/Docker. Redis is optional and not used for the async queue. Async media has **only private CF R2 persistence**, never a local durable directory or provider-URL fallback. Existing relay code may use bounded, temporary request buffers; those are not persistent async recovery storage.

## Business ownership and API-key permissions

Each first-level business should have its own New API account. Use that account's management access token (`nap_...`) for `/api/business/*`; the current middleware derives ownership from the authenticated token and does not require `New-Api-User`. Browser sessions work as before. Generation keys (`sk-...`) cannot create/manage keys. Management access tokens must explicitly have the appropriate profile, key or task scopes; administrators do not gain cross-account result access through these owner-scoped endpoints.

`tag_level_1` / `tag_level_2` are optional, independently editable, case-sensitive labels, not a hierarchy or wildcard. The exact account+label-pair defines result access. Empty labels select an exact empty-label domain. A rotated generation key with the same account+labels can read unexpired results. Editing labels changes that key's future access, but does not move old tasks or usage; the UI warns before saving. Owner management APIs can read all that owner's domains.

`business_system_id` belongs to the account; update with `PUT /api/business/profile` and `{"business_system_id":"system-a"}`. Per-call `business_id`, `external_user_id`, `external_task_id` are attribution only, never authorization. Tasks and consumption logs freeze the submission-time labels and account identifier.

Create a business generation key:

```http
POST /api/business/keys
Authorization: Bearer <management-access-token>
Content-Type: application/json

{
  "name": "product-worker",
  "expired_time": -1,
  "remain_quota": 100000,
  "unlimited_quota": false,
  "tag_level_1": "subsystem-b",
  "tag_level_2": "product-1",
  "allowed_modalities": ["image", "video", "audio"]
}
```

The successful `data` contains the new ID and full key. Treat the full key as a secret; never log it. Existing model restrictions, groups, expiration and quota fields also apply. An empty/omitted `allowed_modalities` preserves legacy unrestricted behavior; valid values are `text`, `image`, `video`, `audio`. Update through `PUT /api/business/keys` using the existing token-update body including `id`. Omitted labels/modalities keep their old values; explicit empty labels/array clear them. Creating and revealing a full key require both write and reveal privileges.

Standard sync/SSE generation accepts attribution headers `X-Business-ID`, `X-External-User-ID`, `X-External-Task-ID`. Existing key forms/lists and consumption logs expose label filtering and attribution. Log queries accept `tag_level_1`, `tag_level_2`, `business_system_id`, `business_id`, `external_user_id`, `external_task_id`, `async_task_id`; supplied empty labels match the empty-label domain rather than all labels.

## Submit and retrieve

```http
POST /v1/async/tasks
Authorization: Bearer <generation-key>
Idempotency-Key: order-20261006-0001
Content-Type: application/json

{
  "endpoint": "/v1/images/generations",
  "retention_seconds": 604800,
  "request": {"model":"your-configured-image-model","prompt":"A landscape","n":1},
  "business_id": "order-0001",
  "external_user_id": "customer-8",
  "external_task_id": "my-task-100"
}
```

After authentication, validation and durable R2 input+SQL task creation, response is `202` with `id`, status, `retention_seconds` and a `Location` header. It is not a promise that provider generation succeeded. Use the same exact idempotency key within the account+label domain to receive the same task; different parameters/attribution/normalized retention return `409`. A business ID is not automatically an idempotency key. No arbitrary upstream URL, proxy endpoint or streaming request is accepted.

`retention_seconds` is optional in the JSON envelope, not inside the provider `request`. Omitted or JSON `null` selects **604800 seconds (7 days)** for a new task. Explicit values must be integers from **600 seconds (10 minutes) through 2592000 seconds (30 days)**; smaller, fractional or larger values are rejected before uploading or queuing. For example, `600` keeps a result for ten minutes, `86400` for one day, and `1209600` for fourteen days. The duration is frozen on the task and its deadline is calculated from completion, not submission or the latest poll. Submit/query responses expose the duration and completed tasks expose `result_expires_at` as a Unix timestamp. Expiry blocks both structured results and artifact downloads immediately; background deletion may lag. Unlimited retention is not supported.

Omission and explicit `604800` are the same normalized request for new-task idempotency. Tasks accepted by the previous 24-hour implementation keep their existing deadline: a replay with omitted retention (or explicit `86400`) returns the original task, not a fresh seven-day result; asking to change that old task's retention conflicts. Legacy queued tasks without a retention snapshot also keep the old one-day behavior. This upgrade does not extend existing stored media.

Allowed operations: `/v1/images/generations`, `/v1/images/edits`, `/v1/videos`, `/v1/audio/speech`, `/v1/audio/transcriptions`, `/v1/audio/translations`. Actual availability still depends on the configured model/channel and existing adapters. For edits/transcription/translation, submit `multipart/form-data`: metadata fields `endpoint`, optional integer text field `retention_seconds`, `business_id`, `external_user_id`, `external_task_id`, plus normal generation fields (`model`, `prompt`, `image`, `file`, etc.). The server removes envelope metadata, including retention, before the existing adapter receives the body. Duplicate retention fields or a retention file part are rejected. Text stays synchronous/SSE. Arbitrary plugin/native routes are not automatically admitted by this API.

Media inputs must be actual uploaded bytes or supported inline data/base64. Remote media URLs and provider file IDs are rejected before storage/admission, including nested JSON extension fields and multipart non-file metadata; otherwise a queued request could depend on an expiring external input instead of its durable R2 input. URLs in ordinary prompt/instruction text remain text. Vendor operations requiring URL-only media inputs are not supported by this first wrapper; their original synchronous/native APIs remain unchanged.

```http
GET /v1/async/tasks/<id>
GET /v1/async/tasks/<id>/result
GET /v1/async/tasks/<id>/artifacts/<artifact_id>
Authorization: Bearer <generation-key-in-the-same-domain>
```

Owner management equivalents are `/api/business/tasks/<id>`, `/result`, `/artifacts/<artifact_id>` with management authorization. Cross-account/domain reads return `404`. Completed structured results contain authenticated artifact routes, never durable public R2 URLs. Files are private, streamed with `no-store`, attachment and `nosniff` headers. Do not expose public bucket access or cache these routes at a CDN. Copy results to your own permanent asset storage before expiry if needed.

`/result` is structured JSON, including for binary speech output or transcription formats such as plain text/SRT. Such outputs return an artifact descriptor with MIME type, size and authenticated download route; `/artifacts/<artifact_id>` returns the original bytes. JSON transcription and standard OpenAI image result shapes retain their structured fields. Submit non-streaming media requests; both `stream: true` and audio `stream_format: "sse"` are rejected by the new wrapper.

States: `queued`, `submitting`, `polling`, `storage_pending`, `completed`, `failed`, `unknown`. Poll with backoff (for example 2–5 seconds). `409` means no completed result, `410` means expired. `unknown` means the request may have reached a provider or the result cannot safely be reconstructed; reconcile it, **do not automatically submit a new generation**. There is no business-level regeneration or cross-model fallback. Existing definite provider rejection can use the established channel retry; uncertain transport/response failures are not blindly resent.

The result-retention duration is not an upstream generation timeout. A submission with an unconfirmed outcome retains recovery evidence for at least 24 hours, independently of a shorter requested result duration. If its accepted provider task is recovered while still generating, the temporary unknown-outcome deadline is cleared and active task inputs/receipts/snapshots are not cleaned as expired results. When completion is confirmed, the generated result uses its original per-task duration measured from the confirmed completion time. This recovery window does not grant access to a completed result beyond its requested deadline.

Workers recheck current account/key/model/modality rights before generation. Already-submitted tasks recover provider polling or saved-result archival, not a fresh submission. A SQL lease/version prevents concurrent owners; expired in-flight submissions become `unknown`. R2 archival retries never regenerate. Charging reserves/settlement/refunds have a SQL transaction journal. A durable outbox retries usage delivery (including an independent log database); dashboard usage is idempotent per job and old sync dashboard aggregation remains unchanged. Compact recovery logs may lack provider-specific token detail if a process died before its detailed callback, but the final quota comes from the journal. This first async release requires Redis quota caching and batch quota updates to be disabled; an incompatible billing-cache configuration returns explicit `503 unsupported_billing_configuration`, while existing synchronous routes continue unchanged.

For accepted native provider tasks, private R2 receipts retain the upstream task ID and immutable R2 snapshots retain adapter state. SQL keeps only the reference and compact task summary. SQL/save failure after acceptance prohibits another generation request: recovery restores the receipt and polls the original provider task, or marks it unknown for reconciliation. Existing non-wrapper native tasks keep their established storage behavior. Legacy Midjourney public image links remain compatible; newly created tasks require authenticated access to the exact business domain, even when both labels are empty.

Use SQLite, MySQL or PostgreSQL for the usage log database. ClickHouse does not provide the unique event constraint needed by this initial delivery implementation, so selecting it explicitly disables new async submission with `503 unsupported_log_database`; existing synchronous ClickHouse logging is unaffected.

## Private R2 and container configuration

See `compose.async.yml`. It builds the existing image, binds the HTTP port to loopback for your reverse proxy and uses only transient tmpfs inside the container; no persistent media volume, Redis or MySQL service is added. It is a template, not a production deployment already executed. Supply secrets through your deployment environment/secret manager, never commit them or paste full `docker compose config` output.

Required async settings: `ASYNC_R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com`, `ASYNC_R2_BUCKET`, `ASYNC_R2_ACCESS_KEY_ID`, `ASYNC_R2_SECRET_ACCESS_KEY`. Optional prefix defaults to `new-api-async`. Scope the credential to read/write/delete the private bucket. Missing/partial/invalid configuration disables new async submission with a safe `503`, without affecting sync APIs. R2 connectivity/upload errors fail before generation; result-save errors do not trigger a new generation.

`ASYNC_TASK_WORKERS` defaults to 2, maximum 16 **per instance**. `ASYNC_TASK_MAX_PENDING_PER_ACCOUNT` defaults to 100; increase only with capacity planning. The current wrapper bounds request, captured response and each archived artifact to 64 MiB; the storage/download primitive also has a separate absolute 256 MiB safety ceiling, which does not increase the wrapper limit. Buffers occupy RAM, so large files/concurrency need more memory; this is not an unconditional 1C1G capacity guarantee. Downloads always apply public-network HTTP(S)/port checks, DNS/IP SSRF protection and a bounded redirect policy, independent of legacy relaxed fetch settings.

New results expire by default **7 days after completion**, or at the per-task `retention_seconds` deadline (10 minutes–30 days); API access stops immediately even if deletion is delayed. Inputs/outputs/raw responses/orphan uploads are batch-deleted with retries. The existing 24-hour queue-start timeout is unrelated to result retention and remains unchanged. Compact job summaries retain 30 days after completion; immutable accounting journals remain for reconciliation. Unknown/unfinalized billing or pending usage delivery may keep compact internal records longer, without exposing expired media. R2 lifecycle deletion uses object upload age, not job completion; configure only a generously longer safety backstop than the longest permitted retention plus in-flight processing time. Do not use a blanket seven-day deletion rule when requests can retain results for thirty days, or use lifecycle deletion as the exact per-task authorization clock. This code does not automatically edit bucket lifecycle rules. See [R2 object lifecycle behavior](https://developers.cloudflare.com/r2/buckets/object-lifecycles/).

For multiple instances use shared PostgreSQL + the same R2 bucket/prefix and shared `SESSION_SECRET` / `CRYPTO_SECRET`; no sticky routing is needed for these async jobs. Startup migration should run on a designated master before starting additional instances. Prefer the Supabase direct connection where network supports it, or Session Pooler; TLS verification must remain enabled. Default pools are 5 open / 2 idle **per process and per database**; multiply this by instance count and any independent log pool. Use a dedicated non-public application schema/database credential. See [Supabase connection options](https://supabase.com/docs/guides/database/connecting-to-postgres). Free storage/connection limits and backend compute remain real constraints; this feature does not make the Go server free or unlimited.

## Verification and operational limits

Backend tests include ledger rollback/idempotency, submission replay/leases, admission canonicalization, domain isolation, revoked key behavior, R2 signature/path/redirect limits, expiry and archive-only retry. Real SQLite, MySQL 5.7 and PostgreSQL fixtures validate fresh migration, upgrade from official `v1.0.0-rc.41`, double migration and legacy data preservation. Independent log fixtures verify nullable event IDs and duplicate delivery.

R2 HTTP contract tests use an isolated test server, not a real Cloudflare account. Before production, perform a staging roundtrip with your private R2 credentials, real configured providers and Supabase network/TLS. No production config, key, bucket or deployment is changed by these checks. Third-party URL downloads that are private-network-only are intentionally rejected; mirror those assets through an approved public source instead of disabling the protection.

### Recorded checks (2026-10-06)

- Backend `go test ./... -timeout=600s`: passed. Final admission/result/billing refinements were additionally checked with affected async/business suites and full model/service tests.
- Independent `relaykit` tests and build with `GOWORK=off`: passed. No root-project dependency was introduced.
- Real SQLite, MySQL 5.7 and PostgreSQL 17: fresh/upgrade/double migration, independent log upgrade, exact labels, concurrent lease winner and journal/log replay checks passed. Fixtures used only explicitly named, task-owned localhost databases.
- Frontend typecheck, lint of the 26 changed TypeScript files and build: passed. Full frontend run: **2172 passed / 1 failed (2173 total; 174 passing files / 1 failing file)**. The unchanged audit viewer timed out looking up the token-create cell; the same file independently passed **32/32** without changing its assertion or timeout. The full-run failure is retained here, not reported as an all-green run. Full-project lint also reports existing unrelated findings; no blanket lint fixes were applied.
- Windows and Linux binary builds, Compose template validation with explicitly fictitious credentials, formatting and `git diff --check`: passed. The Docker image itself was not deployed or used to modify any live service.

Regression fixtures also cover native acceptance followed by SQL-save failure, signed R2 receipt HEAD/GET recovery, terminal billing-CAS recovery, key deletion/revocation, binary result/archive-only retries, unknown recovery fairness, no-success-count refunds, 24-hour access denial/cleanup and persistent accounting records. Two unchanged HTTP2 tests were flaky on the Windows `03c85e8` baseline due to closing unread TCP connections immediately after GOAWAY/RST; only their test fixture was adjusted to drain safely. The complete channel test package subsequently passed ten consecutive runs; production transport code was not changed.

The implementation checks did not deploy production, write to a real R2 bucket or connect to Supabase. Publishing the feature branch is separate from deployment; real provider/R2/Supabase staging validation remains required before rollout.

### Per-task retention update checks (2026-10-06)

- Initial retention implementation: `go test ./... -timeout=600s` passed (controller 359.093s, model 39.472s, service 2.705s). After independent review, short-retention recovery protection and old/new-instance idempotency-race handling were added and checked separately against the final code below; the initial full-suite result is not presented as a second full-suite run.
- Final controller contract regression: `go test ./controller -run '^TestAsyncAdmission|^TestAsyncTaskRead' -count=1 -timeout=90s` passed (0.609s). Covers default/null/custom/bounds, JSON and multipart idempotency, legacy replay and a real ORM-callback simulation of an old instance winning admission after the new instance's fast lookup.
- Before raising the minimum to ten minutes, complete affected backend packages: `go test ./model ./service -timeout=600s` passed (model 23.414s, service 1.547s). Async recovery regression verified the then-permitted one-minute output duration did not remove pending provider evidence or send another generation; confirmed output still expired at completion plus one minute.
- Explicit synthetic localhost DSNs plus `go test ./model -run '^TestAsyncRetentionDatabaseMigrationMatrix$' -count=1 -timeout=180s -v`: passed for real SQLite **3.50.4**, MySQL **5.7.44** and PostgreSQL **17.11** (final three-engine run 34.934s). Each engine exercised fresh creation, upgrade from the `2951360` AsyncJob schema, two startup migrations with close/reopen, nullable legacy retention, original deadlines and unique indexes, duration roundtrips and per-row expiration CAS. Only new task-owned test databases were used.
- Final Windows build `go build -buildvcs=false -o .local-tests/new-api-retention.exe .` and Linux/amd64 build with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` passed. Local plain builds first failed Git VCS metadata stamping due to checkout ownership; `-buildvcs=false` only disables that local build stamp, not source checks or tests. Independent `relaykit` `GOWORK=off go build ./...` also passed.
- `gofmt` and `git diff --check` passed. No frontend sources changed in this retention update; the earlier frontend test caveat remains as recorded above. No commit, push, deployment, real R2 bucket/lifecycle change or Supabase connection was performed for this update. Restored task-owned database containers were stopped again without deleting their data.

### Ten-minute minimum follow-up (2026-10-06)

- New-task validation now accepts `retention_seconds` from **600 through 2592000**; default retention remains **604800**. JSON and multipart regressions verify 600 is accepted, 599 and the former 60-second minimum are rejected before upload/queueing, and legacy replay preserves saved deadlines. Controller admission/result tests passed (0.969s).
- Complete model/service tests passed (17.726s / 1.857s), including short-retention recovery, completion-plus-600 expiry and preservation of existing absolute deadlines. SQLite fresh/upgrade migration tests passed (8.520s). There is no schema change in this follow-up; the previous real MySQL/PostgreSQL results above were not rerun.
- Windows backend build with `-buildvcs=false`, formatting and diff checks passed. These local changes remain uncommitted; no push or production configuration was performed.
