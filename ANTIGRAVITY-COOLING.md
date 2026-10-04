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

The original Google 429 remains unexplained.
This override permits an explicit project choice; it does not prove native/proxy project equivalence.

## Optional local policy

The canonical `upstream.antigravity.model-level-cooling` setting defaults to false.
Legacy `antigravity.model-level-cooling` also works.
Missing or false preserves the previous policy, including credential-wide cooling for bare resource exhaustion.
The option does not disable cooling or grant additional Google quota.

```yaml
upstream:
  antigravity:
    model-level-cooling: true
```

When enabled, ambiguous resource exhaustion holds the failed canonical model rather than every model on the credential.
The failed model keeps the five-minute fallback.
Longer explicit provider reset guidance remains intact.
Explicit quota rejection retains conservative existing handling unless supported evidence establishes narrower scope.
Aliases and thinking suffixes cannot bypass the same canonical model's hold.
Existing active credential holds are not cleared merely because this option changes.

This limits local outage amplification; it does not establish why Google's response differs from native `agy`.
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
