# Anthropic HTTP Cache Observations

## Background

This note summarizes the current behavior of the local `Claude Code -> /v1/messages -> gateway-openai -> sub2api /v1/responses` path as validated on March 30, 2026.

The current architecture is:

- `gateway-openai` handles Anthropic compatibility locally
- upstream `sub2api` is treated as an OpenAI Responses provider
- the active path is HTTP/SSE full replay, not response-level continuation

This note intentionally focuses on observed cache behavior, not general protocol design.

## Responsibility Boundary

### Provider

The actual prompt/prefix cache lives in the upstream provider.

Examples:

- `sub2api`
- OpenAI / ChatGPT upstream

The provider decides:

- whether cache exists
- which prefix qualifies for cache reuse
- how much of the request is counted as cache read

### `gateway-openai` plugin

The plugin is the main cache-chain control layer.

It is responsible for:

- Anthropic → Responses request transformation
- extracting and persisting stable session anchors
- injecting `prompt_cache_key` / session-related fields into upstream requests
- parsing upstream `cached_tokens` / `cached_input_tokens`
- shaping replay requests so the provider has a chance to reuse cache

### `airgate-core`

Core does not implement prompt cache behavior itself.

Core is responsible for:

- auth
- scheduling
- rate limiting
- billing
- usage / outcome recording

Core only consumes cache-related numbers already parsed by the plugin, such as `CachedInputTokens`.

## Current Chain

1. Claude Code sends Anthropic `/v1/messages`
2. `gateway-openai` converts Anthropic request to OpenAI Responses request
3. `gateway-openai` injects a stable `prompt_cache_key` into the Responses body for API key upstreams
4. request is sent to `sub2api /v1/responses`
5. usage is parsed back from Responses SSE / completed events

## Confirmed Improvements

### Stable cache anchor is now present

For the main long-running session, the following fields remained stable across turns:

- `prompt_cache_key`
- `session_key`
- `system_hash`
- `tool_choice_hash`
- `tools_hash`

The request body also now explicitly contains `prompt_cache_key`:

- `body_has_prompt_cache_key=true`

This was the main fix that materially improved cache hits for `gpt-5.4`.

### `gpt-5.4` cache hits are now real and substantial

Observed examples:

- `input_tokens=19583`, `cached_tokens=19456`
- `input_tokens=26043`, `cached_tokens=24576`
- `input_tokens=34667`, `cached_tokens=33408`
- `input_tokens=56567`, `cached_tokens=56448`

This confirms provider-side prefix cache is working on the current HTTP path.

## Remaining Behavior

### Some turns still show `cached_tokens=0`

Observed examples:

- `input_tokens=19479`, `cached_tokens=0`
- `input_tokens=19543`, `cached_tokens=0`
- `input_tokens=24698`, `cached_tokens=0`

However, these zero-hit turns are often followed by large-hit turns in the same session.

This strongly suggests:

- cache linkage is no longer fundamentally broken
- zero-hit turns are more likely cache warmup / prefix expansion misses
- the provider is able to reuse cache on subsequent turns once the expanded prefix has been seen

### Full replay is still the dominant mode

Current request mode remains:

- `request_mode=full_replay`
- `request_reason=continuation_disabled`

This means:

- every turn still replays the full request history
- new history growth must first be written before later turns can read from cache
- some "first expanded turn miss, next turn hit" behavior is expected

## Most Likely Explanation For Residual Misses

The remaining misses are no longer explained by top-level anchor instability.

The most likely explanation is:

- full replay keeps appending new tool-call / tool-result blocks
- provider cache can reuse the stable prefix
- but newly expanded suffixes must first be warmed before they become cache-readable on later turns

In short:

- not a broken cache chain
- more a consequence of HTTP full replay without true continuation

## Important Non-Causes

The following are unlikely to be the primary cause of the current residual misses:

- `prompt_cache_key` drift
- `system` prompt drift
- `tool_choice` drift
- tool set drift

These values were observed to remain stable in the debug logs.

## Practical Interpretation

Current state is:

- cache behavior is materially improved
- major sessions can now achieve large cache reuse
- residual misses should be interpreted as provider-side prefix warmup / replay growth behavior, not as a primary integration bug

## Recommended Next Steps

### Safe next steps that preserve context

1. Keep the current `prompt_cache_key` injection for Anthropic -> Responses API-key upstreams.
2. Continue observing hit rate on real sessions before changing replay semantics.
3. Prefer small request-stability improvements over aggressive history trimming.

### Avoid immediately

1. Aggressive context trimming that sacrifices useful tool history.
2. Reintroducing `previous_response_id` on the HTTP path without a provider path that guarantees continuation semantics.

### Future optimization directions

1. Continue request-shape stabilization for full replay.
2. Evaluate provider-side continuation only if a transport path with clear semantics is available.
3. Only consider stronger trimming after confirming the remaining misses are materially costly in real workflows.
