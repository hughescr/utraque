package providerreport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/leg"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/referenceprice"
	"github.com/hughescr/utraque/internal/usagehistory"
)

// The expected documents below were generated from the schema-v1 collector
// before the report vocabularies (Status, Section, ErrorCode,
// UnavailableReason) were typed and before reference prices moved to
// PriceRow. They pin the wire bytes: keys, key order, values and omitempty
// behaviour. Regenerate only with a schema bump.
const (
	wireMixed  = `{"schema_version":1,"generated_at":"2026-09-11T12:05:00Z","collection_started_at":"2026-09-11T12:05:00Z","collection_ended_at":"2026-09-11T12:05:00Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[],"quota_after":{"source":"anthropic","collected_at":"2026-09-11T12:05:00Z","quotas":[{"id":"five_hour","used_percent":31.5,"unit":"percent_0_100","resets_at":"2026-09-11T13:05:00Z"}]},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"blocks":[],"issues":null},"reference_prices":{"source":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"claude-sonnet-5","input":2,"output":10,"cache_write":2.5,"eligible":true}],"assumptions":["cache_write_5m"]},"calibration":{"unavailable_reason":"paired_quota_measurement_unavailable"},"configured_plan":{"label":"max","multiplier":5,"source":"configured"}},{"provider":"codex","status":"partial","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota_after","code":"credential_unavailable","retryable":false,"message":"report source unavailable"}],"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"source":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"gpt-5.6-sol","input":4,"output":20,"cache_read":0.4,"eligible":true},{"model":"old-gpt","input":1,"output":2,"eligible":false}],"assumptions":["base_tier"]}},{"provider":"deepseek","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[],"quota_after":{"source":"deepseek","collected_at":"2026-09-11T12:05:00Z","balances":[{"kind":"","currency":"USD","total":"2.50"},{"kind":"","currency":"CNY","total":"10"},{"kind":"","currency":"USD","total":"abc"}],"available":true},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"source":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"deepseek-v4","input":0.5,"output":1,"eligible":false}]},"conditional_remaining_token_estimates":[{"model":"deepseek-v4","currency":"USD","balance":"2.50","tokens":250,"historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"]},{"model":"deepseek-x","currency":"USD","balance":"2.50","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"historical_effective_rate_unavailable"},{"model":"deepseek-v4","currency":"CNY","balance":"10","historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"unsupported_balance_currency"},{"model":"deepseek-x","currency":"CNY","balance":"10","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"unsupported_balance_currency"},{"model":"deepseek-v4","currency":"USD","balance":"abc","historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"invalid_balance_decimal"},{"model":"deepseek-x","currency":"USD","balance":"abc","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"historical_effective_rate_unavailable"}]}],"unattributed_history":[{"source":"opencode","model":"mystery","provider":"unknown","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":7,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]}`
	wireFailed = `{"schema_version":1,"generated_at":"2026-09-11T12:05:00Z","collection_started_at":"2026-09-11T12:05:00Z","collection_ended_at":"2026-09-11T12:05:00Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"error","last_attempt":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota_after","code":"rate_limited","retryable":true,"retry_at":"2026-09-11T12:09:00Z","message":"provider reading unavailable"},{"section":"history","code":"timeout","retryable":true,"message":"local usage history unavailable"},{"section":"reference_prices","code":"unavailable","retryable":true,"message":"reference prices unavailable"}],"calibration":{"unavailable_reason":"paired_quota_measurement_unavailable"}},{"provider":"codex","status":"error","last_attempt":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota_after","code":"credential_unavailable","retryable":false,"message":"report source unavailable"},{"section":"history","code":"timeout","retryable":true,"message":"local usage history unavailable"},{"section":"reference_prices","code":"unavailable","retryable":true,"message":"reference prices unavailable"}]},{"provider":"deepseek","status":"error","last_attempt":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota_after","code":"configuration_error","retryable":false,"message":"report source unavailable"},{"section":"history","code":"timeout","retryable":true,"message":"local usage history unavailable"},{"section":"reference_prices","code":"unavailable","retryable":true,"message":"reference prices unavailable"}]}]}`
	wireStale  = `{"schema_version":1,"generated_at":"2026-09-11T12:05:00Z","collection_started_at":"2026-09-11T12:05:00Z","collection_ended_at":"2026-09-11T12:05:00Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":true,"age_seconds":3},"errors":[],"quota_after":{"source":"anthropic","collected_at":"2026-09-11T12:05:00Z","quotas":[{"id":"five_hour","used_percent":31.5,"unit":"percent_0_100","resets_at":"2026-09-11T13:05:00Z"}]},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"blocks":[],"issues":null},"reference_prices":{"source":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"claude-sonnet-5","input":2,"output":10,"cache_write":2.5,"eligible":true}],"assumptions":["cache_write_5m"]},"calibration":{"unavailable_reason":"cached_measurement_expired"},"configured_plan":{"label":"max","multiplier":5,"source":"configured"}},{"provider":"codex","status":"partial","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":true,"age_seconds":3},"errors":[{"section":"quota_after","code":"credential_unavailable","retryable":false,"message":"report source unavailable"}],"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"source":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"gpt-5.6-sol","input":4,"output":20,"cache_read":0.4,"eligible":true},{"model":"old-gpt","input":1,"output":2,"eligible":false}],"assumptions":["base_tier"]},"calibration":{"unavailable_reason":"cached_measurement_expired"}},{"provider":"deepseek","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":true,"age_seconds":3},"errors":[],"quota_after":{"source":"deepseek","collected_at":"2026-09-11T12:05:00Z","balances":[{"kind":"","currency":"USD","total":"2.50"},{"kind":"","currency":"CNY","total":"10"},{"kind":"","currency":"USD","total":"abc"}],"available":true},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"source":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"deepseek-v4","input":0.5,"output":1,"eligible":false}]},"calibration":{"unavailable_reason":"cached_measurement_expired"}}],"unattributed_history":[{"source":"opencode","model":"mystery","provider":"unknown","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":7,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]}`
	wireReset  = `{"schema_version":1,"generated_at":"2026-09-11T14:05:00Z","collection_started_at":"2026-09-11T12:05:00Z","collection_ended_at":"2026-09-11T12:05:00Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":true,"age_seconds":0},"errors":[],"quota_after":{"source":"anthropic","collected_at":"2026-09-11T12:05:00Z","quotas":[{"id":"five_hour","used_percent":31.5,"unit":"percent_0_100","resets_at":"2026-09-11T13:05:00Z"}]},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"blocks":[],"issues":null},"reference_prices":{"source":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"claude-sonnet-5","input":2,"output":10,"cache_write":2.5,"eligible":true}],"assumptions":["cache_write_5m"]},"calibration":{"unavailable_reason":"quota_window_reset_after_collection"},"configured_plan":{"label":"max","multiplier":5,"source":"configured"}},{"provider":"codex","status":"partial","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota_after","code":"credential_unavailable","retryable":false,"message":"report source unavailable"}],"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"source":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"gpt-5.6-sol","input":4,"output":20,"cache_read":0.4,"eligible":true},{"model":"old-gpt","input":1,"output":2,"eligible":false}],"assumptions":["base_tier"]}},{"provider":"deepseek","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[],"quota_after":{"source":"deepseek","collected_at":"2026-09-11T12:05:00Z","balances":[{"kind":"","currency":"USD","total":"2.50"},{"kind":"","currency":"CNY","total":"10"},{"kind":"","currency":"USD","total":"abc"}],"available":true},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"source":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"deepseek-v4","input":0.5,"output":1,"eligible":false}]},"conditional_remaining_token_estimates":[{"model":"deepseek-v4","currency":"USD","balance":"2.50","tokens":250,"historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"]},{"model":"deepseek-x","currency":"USD","balance":"2.50","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"historical_effective_rate_unavailable"},{"model":"deepseek-v4","currency":"CNY","balance":"10","historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"unsupported_balance_currency"},{"model":"deepseek-x","currency":"CNY","balance":"10","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"unsupported_balance_currency"},{"model":"deepseek-v4","currency":"USD","balance":"abc","historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"invalid_balance_decimal"},{"model":"deepseek-x","currency":"USD","balance":"abc","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"historical_effective_rate_unavailable"}]}],"unattributed_history":[{"source":"opencode","model":"mystery","provider":"unknown","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":7,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]}`
	wireCached = `{"schema_version":1,"generated_at":"2026-09-11T12:05:02Z","collection_started_at":"2026-09-11T12:05:02Z","collection_ended_at":"2026-09-11T12:05:02Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"error","last_attempt":"2026-09-11T12:05:02Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota_after","code":"credential_unavailable","retryable":false,"message":"report source unavailable"},{"section":"history","code":"unavailable","retryable":false,"message":"report source unavailable"}],"calibration":{"unavailable_reason":"paired_quota_measurement_unavailable"}},{"provider":"codex","status":"error","last_attempt":"2026-09-11T12:05:02Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota_after","code":"credential_unavailable","retryable":false,"message":"report source unavailable"},{"section":"history","code":"unavailable","retryable":false,"message":"report source unavailable"}]},{"provider":"deepseek","status":"error","last_attempt":"2026-09-11T12:05:02Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota_after","code":"unavailable","retryable":false,"message":"report source unavailable"},{"section":"history","code":"unavailable","retryable":false,"message":"report source unavailable"}],"last_complete_snapshot":{"last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":true,"age_seconds":2},"quota_after":{"source":"deepseek","collected_at":"2026-09-11T12:05:00Z"},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","source":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01}]},"blocks":[],"issues":null},"calibration":{"unavailable_reason":"cached_measurement_expired"}}}]}`
)

func wireHistory(now time.Time) usagehistory.Report {
	history := sampleHistory(now)
	history.Daily = append(history.Daily,
		usagehistory.DailyModelUsage{Date: utcDate(now), Source: "claude", Model: "deepseek-x", InferredLeg: leg.DeepSeek, TotalTokens: 5, CostStatus: usagehistory.CostUnavailableOrUnpriced},
		usagehistory.DailyModelUsage{Date: utcDate(now), Source: "claude", Model: "old-gpt", InferredLeg: leg.Codex, TotalTokens: 10},
		usagehistory.DailyModelUsage{Date: utcDate(now), Source: "opencode", Model: "mystery", InferredLeg: leg.Unknown, TotalTokens: 7})
	return history
}

func wirePrices(now time.Time) referenceprice.Snapshot {
	cacheRead, cacheWrite := .4, 2.5
	return referenceprice.Snapshot{Source: referenceprice.SourceModelsDev, ObservedAt: now, Unit: referenceprice.USDPerMillion,
		Models: []referenceprice.ModelPrice{
			{Provider: "codex", Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: &cacheRead, HasHigherTier: true},
			{Provider: "codex", Model: "old-gpt", Input: 1, Output: 2},
			{Provider: "codex", Model: "unseen-gpt", Input: .1, Output: .2},
			{Provider: "anthropic", Model: "claude-sonnet-5", Input: 2, Output: 10, CacheWrite: &cacheWrite},
			{Provider: "deepseek", Model: "deepseek-v4", Input: .5, Output: 1},
		}}
}

func wireEligible(id leg.ID) []string {
	switch id {
	case leg.Codex:
		return []string{"gpt-5.6-sol"}
	case leg.Anthropic:
		return []string{"claude-sonnet-5"}
	}
	return nil
}

// wireMixedReport collects a report exercising every status, every section,
// sentinel and quota-derived error codes, eligible and history-only price rows,
// and every RemainingEstimate unavailable reason.
func wireMixedReport(t *testing.T, now time.Time) Report {
	t.Helper()
	reset := now.Add(time.Hour)
	available := true
	h := New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) { return wireHistory(now), nil }),
		Anthropic: anthropicFunc(func(context.Context, string) (providerquota.Observation, error) {
			return providerquota.Observation{Source: leg.Anthropic, CollectedAt: now,
				Quotas: []providerquota.Quota{{ID: "five_hour", UsedPercent: 31.5, Unit: "percent_0_100", ResetsAt: &reset}}}, nil
		}),
		DeepSeek: deepSeekFunc(func(context.Context) (providerquota.Observation, error) {
			return providerquota.Observation{Source: leg.DeepSeek, CollectedAt: now, Available: &available,
				Balances: []providerquota.Balance{{Currency: "USD", Total: "2.50"}, {Currency: "CNY", Total: "10"}, {Currency: "USD", Total: "abc"}}}, nil
		}),
		ReferencePrices:     priceFunc(func(context.Context) (referenceprice.Snapshot, error) { return wirePrices(now), nil }),
		EligiblePriceModels: wireEligible,
		ClaudePlan:          "max", ClaudePlanMultiplier: ptr(5.0),
		Now: func() time.Time { return now },
	})
	return h.collect(context.Background(), credentials{anthropicToken: "token"}, utcDate(now).AddDate(0, 0, -29), utcDate(now))
}

// wireFailedReport collects a report in which every section fails: a
// rate-limited quota error, a history CollectError, a reference-price Error and
// the two readQuotas sentinels.
func wireFailedReport(t *testing.T, now time.Time) Report {
	t.Helper()
	retryAt := now.Add(4 * time.Minute)
	h := New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			return usagehistory.Report{}, &usagehistory.CollectError{Section: "daily", Kind: usagehistory.ErrorTimeout}
		}),
		Anthropic: anthropicFunc(func(context.Context, string) (providerquota.Observation, error) {
			return providerquota.Observation{}, &providerquota.Error{Provider: leg.Anthropic, Code: providerquota.CodeRateLimited, Retryable: true, RetryAt: &retryAt}
		}),
		ReferencePrices: priceFunc(func(context.Context) (referenceprice.Snapshot, error) {
			return referenceprice.Snapshot{}, &referenceprice.Error{Code: referenceprice.CodeUnavailable, Retryable: true}
		}),
		Now: func() time.Time { return now },
	})
	return h.collect(context.Background(), credentials{anthropicToken: "token"}, utcDate(now).AddDate(0, 0, -29), utcDate(now))
}

// wireCachedBody serves a report twice through the handler with the DeepSeek
// reader and history failing on the second collection, so the second body
// carries a last_complete_snapshot.
func wireCachedBody(t *testing.T, now time.Time) []byte {
	t.Helper()
	failing := false
	h := New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			if failing {
				return usagehistory.Report{}, errors.New("failed")
			}
			return sampleHistory(now), nil
		}),
		DeepSeek: deepSeekFunc(func(context.Context) (providerquota.Observation, error) {
			if failing {
				return providerquota.Observation{}, errors.New("failed")
			}
			return providerquota.Observation{Source: leg.DeepSeek, CollectedAt: now}, nil
		}),
		CacheTTL: time.Second, Now: func() time.Time { return now }})
	request := func() []byte {
		r := httptest.NewRequest(http.MethodGet, "/v1/utraque/providers", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	request()
	now = now.Add(2 * time.Second)
	failing = true
	return request()
}

func TestReportWireBytesUnchanged(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	mixed := wireMixedReport(t, now)
	stale := wireMixedReport(t, now)
	markFreshness(&stale, true, true, 3*time.Second)
	reset := wireMixedReport(t, now)
	reset.GeneratedAt = now.Add(2 * time.Hour)
	markFreshness(&reset, false, false, 0)
	for _, tc := range []struct {
		name string
		want string
		got  any
	}{
		{"mixed", wireMixed, mixed},
		{"failed", wireFailed, wireFailedReport(t, now)},
		{"stale", wireStale, stale},
		{"reset", wireReset, reset},
		{"cached", wireCached, json.RawMessage(wireCachedBody(t, now))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.got)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tc.want {
				t.Fatalf("wire bytes changed\n got: %s\nwant: %s", body, tc.want)
			}
		})
	}
}
