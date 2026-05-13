# OpenRouter Test Fixture — Provenance

## openrouter_response.json

**Source:** `GET https://openrouter.ai/api/v1/models` (no auth — public catalog endpoint)
**Captured:** 2026-05-12
**Captured by:** Models Discovery initiative, task M2.7a (vendor OpenRouter fixture before M2.8 pricing refresher build)
**Size:** 432,636 bytes (≈422 KB, well under the 1 MB cap)
**Model count at capture:** 365
**Capture command:**

    curl -fsS --max-time 30 'https://openrouter.ai/api/v1/models' \
        -o internal/catalog/testdata/openrouter_response.json

## Why vendored, not live-fetched in tests

CI must not depend on the live OpenRouter API. The response shape is
stable enough across versions that a frozen snapshot exercises every
field `parseOpenRouterPricing` (M2.8) needs to handle:

- `pricing` object with keys: `prompt`, `completion`, `image`, `audio`,
  `request`, `input_cache_read`, `input_cache_write`,
  `internal_reasoning`, `web_search`. Free models price `"0"`, paid
  models price as fixed-point strings (e.g. `"0.000003"` for input
  USD/token; OpenRouter does NOT divide by 1M like our internal catalog).
- `architecture` object with `modality`, `tokenizer`, `instruct_type`,
  `input_modalities` (array), `output_modalities` (array). Capability
  inference for image/audio/thinking will read these.
- `top_provider` object with `context_length`, `max_completion_tokens`,
  `is_moderated`. `top_provider.context_length` is the authoritative
  context window (sometimes differs from the model-level `context_length`).
- `supported_parameters` array (e.g. `["tools", "tool_choice", ...]`).
  Tool-use capability inference reads this.
- Free-tier model IDs end in `:free` (e.g. `inclusionai/ring-2.6-1t:free`).
  The pricing refresher must persist these correctly (zero prices).

## When to recapture

Recapture this fixture if:

- OpenRouter ships a new pricing key (e.g. `input_cache_create` becomes
  a stable field). `parseOpenRouterPricing` should ignore unknown keys
  by design; recapture only needed to add an *opinion* on a new key.
- A failing M2.8 / M3.x test traces to a shape mismatch between the
  fixture and the live API. Recapture, diff old-vs-new with
  `jq -S 'keys'`, and update parser code if the new key is load-bearing.
- Routine periodic refresh — not required, but harmless if done with the
  same `curl` command. Bump the "Captured" date here.

## What NOT to do

- Do NOT trim the fixture to save bytes. The full 432 KB sits well under
  the 1 MB cap and the in-budget cost of running a parse test against
  365 models is negligible (sub-millisecond). Coverage of edge cases
  (free models, missing fields, exotic providers) is worth more than
  the disk space.
- Do NOT scrub or anonymize fields. The endpoint is fully public — no
  PII, no credentials. The capture is a verbatim snapshot.
- Do NOT add an authenticated request. OpenRouter's `/api/v1/models` is
  unauthenticated; adding a key changes nothing in the response and
  introduces a credential dependency the test should not have.
