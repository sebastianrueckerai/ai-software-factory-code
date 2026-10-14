-- the call row's own truth (roadmap 198 item 1, plan
-- docs/plans/2026-10-13-token-cost-per-task.md): the usage the provider
-- reported per call, the modeled pay-as-you-go cost LiteLLM computed from
-- it (NULL: the call reported no figure; under the GLM Coding Plan no
-- money is billed), how long the call ran and the status the upstream
-- answered (0 / '': the call never reached a provider). Tokens are the
-- stored fact; every EUR figure is a modeled equivalent.
ALTER TABLE audit ADD COLUMN IF NOT EXISTS prompt_tokens integer NOT NULL DEFAULT 0,
 ADD COLUMN IF NOT EXISTS cached_tokens integer NOT NULL DEFAULT 0,
 ADD COLUMN IF NOT EXISTS output_tokens integer NOT NULL DEFAULT 0,
 ADD COLUMN IF NOT EXISTS call_cost_eur numeric,
 ADD COLUMN IF NOT EXISTS duration_ms integer NOT NULL DEFAULT 0,
 ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT '';
