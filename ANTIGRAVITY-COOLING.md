# Antigravity cooling scope

The proxy's cooling policy and Google's quota enforcement are separate mechanisms.
A local hold explains requests rejected before reaching Google.
It does not explain Google's original rejection.

## Root-cause status

The reported Flash failure and later Pro success differ in model and time.
They do not prove that native `agy` has a different usable quota bucket.

A bounded diagnostic used the same requested Flash High alias on both clients.
The native CLI exited successfully with request logging directed to `/dev/null`.
Its structured output did not safely expose the actual upstream model or project fields.
The proxy returned HTTP 200, resolved the model to `gemini-3.7-flash`, and returned the expected synthetic answer.
The original Google 429 was not reproduced.

The native generation project, quota project, OAuth identity, credits mode, and exact thinking payload remain unverified.
Do not describe this result as complete wire-level parity.
No project IDs, endpoints, credentials, or authentication identities were changed during diagnostics.

## Important field distinctions

The proxy's stored `project_id` becomes the generation JSON's top-level `project`.
That value is not a quota-project HTTP header.
A native conversation-project label cannot be assumed to identify the same field.

Generation defaults to the daily service.
Existing project discovery uses production `loadCodeAssist`, while onboarding can use daily.
Populated projects remain cached across refreshes.
These differences are confirmed in source, but their role in the reported failure is unproved.
Replacing every project with `default-cli-project` is not justified by the current evidence.

## Explicit generation-project override

An operator can select a generation project without replacing stored OAuth metadata:

```yaml
upstream:
  antigravity:
    project-id: "default-cli-project"
```

The fork default is empty and preserves stored/discovered project selection.
Legacy `antigravity.project-id` and historical `oauth.providers.antigravity.project-id` also work.
The canonical key takes precedence, including an explicit empty value.
Whitespace-only values preserve the existing behavior.

An active override skips project discovery, not normal token acquisition or refresh.
It changes only the generation envelope's `project` selection.
It does not change quota-project headers, OAuth identity, endpoints, credits policy, or local cooldowns.
Google still enforces project authorization and quota.
The changed project can change upstream quota attribution or rejection.
The executor does not fall back to the stored project after Google rejects the override.

Remove the setting, or set it to an empty string, to restore stored/discovered selection.
Home-controlled nodes require the setting in their authoritative Home-delivered provider configuration.
The my-init template supplies `default-cli-project`, but existing live configs remain unchanged.
A compatible binary and explicit configuration rollout are required before running requests use this setting.

This override permits an explicit project choice; it does not prove native/proxy project equivalence.
It did not prevent the bare 429s described below.

## Bare RESOURCE_EXHAUSTED is throttling

Google returns a bare `429 RESOURCE_EXHAUSTED` with no `RetryInfo`, no `ErrorInfo` reason, and no named limit.
On 2026-10-04 the account quota summary (`v1internal:retrieveUserQuotaSummary`) reported the Gemini 5-hour bucket at 100% and the weekly bucket at 94% while such 429s occurred.
Proxy logs since 2026-09-18 show each episode followed a burst of about 5–16 requests per minute.
Retries after 2, 3, 5, 9, 18, 40, and 65 seconds kept receiving 429 for one to four minutes.
Native `agy` receives the same responses; it retries model calls inside the request with exponential backoff and jitter.

The executor therefore treats a bare 429 as follows:

1. It reads the account quota summary, cached for 30 seconds per credential.
2. If an exhausted bucket covers the failed model's group, it holds that model until the bucket's `resetTime`.
   Proto3 JSON omits a zero `remainingFraction`, so a missing value with a future reset counts as exhausted.
3. Otherwise it holds only the failed model for 10, 20, 40, and then 60 seconds, with ±20% jitter.
   A success resets the ladder. The conductor floors supplied delays at 10 seconds.
4. If the summary is unavailable, it uses the same ladder.

The conductor waits for a hold no longer than `max-retry-interval` and retries within the same request, as `agy` does.
Set `request-retry` and `max-retry-interval` (for example 3 and 60) to enable that wait; both default to zero.
Explicit `QUOTA_EXHAUSTED`, credits-balance, and `RATE_LIMIT_EXCEEDED` responses keep their previous handling.
This replaces PR #6335's flat five-minute credential-wide hold for bare responses only.

## Optional local policy

The canonical `upstream.antigravity.model-level-cooling` setting defaults to false.
Legacy `antigravity.model-level-cooling` also works.
Missing or false preserves the previous policy for responses other than a bare `RESOURCE_EXHAUSTED`.
The option does not disable cooling or grant additional Google quota.

```yaml
upstream:
  antigravity:
    model-level-cooling: true
```

When enabled, ambiguous rate limits hold the failed canonical model rather than every model on the credential.
Longer explicit provider reset guidance remains intact.
Explicit quota rejection retains conservative existing handling unless supported evidence establishes narrower scope.
Aliases and thinking suffixes cannot bypass the same canonical model's hold.
Existing active credential holds are not cleared merely because this option changes.

This limits local outage amplification.
Live enablement, release installation, and service restart require a separate operator-approved rollout.

## Validation

Use fake transports and controllable clocks for cooling tests.
Cover legacy defaults, streaming and nonstreaming, canonical aliases, previously unseen siblings, and concurrent failures.
Repeat requests to a held model must make no upstream attempt before expiry.
Keep genuine provider cooldowns monotonic and avoid immediate retry storms.

If the original Google failure returns, collect a matched native/proxy comparison near the same time.
Match the actual model, thinking configuration, account correspondence, endpoint, and billing mode.
Collect only allowlisted nonsecret metadata and structured quota/reset information.
Never enable bearer, cookie, OAuth-record, or user-prompt logging to investigate it.
