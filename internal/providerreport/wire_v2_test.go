package providerreport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/leg"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/usagehistory"
)

// The expected documents below are the schema-2 projections of exactly the
// fixtures wire_test.go pins at schema 1, generated from renderV2 when schema
// 2 was introduced. They pin the schema-2 wire bytes: keys, key order, values
// and omitempty behaviour. Regenerate only with a schema bump.
const (
	wireMixedV2  = `{"schema_version":2,"generated_at":"2026-09-11T12:05:00Z","collection_started_at":"2026-09-11T12:05:00Z","collection_ended_at":"2026-09-11T12:05:00Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[],"quota":{"collected_at":"2026-09-11T12:05:00Z","quotas":[{"id":"five_hour","bucket":"five_hour","kind":"session","used_percent":31.5,"unit":"percent_0_100","resets_at":"2026-09-11T13:05:00Z"}]},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"blocks":[],"issues":null},"reference_prices":{"catalog":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"claude-sonnet-5","input":2,"output":10,"cache_write":2.5,"eligible":true}],"assumptions":["cache_write_5m"]},"calibration":{"unavailable_reason":"paired_quota_measurement_unavailable"},"configured_plan":{"label":"max","multiplier":5,"provenance":"configured"}},{"provider":"codex","status":"partial","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota","code":"credential_unavailable","retryable":false,"message":"report source unavailable"}],"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"catalog":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"gpt-5.6-sol","input":4,"output":20,"cache_read":0.4,"eligible":true},{"model":"old-gpt","input":1,"output":2,"eligible":false}],"assumptions":["base_tier"]}},{"provider":"deepseek","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[],"quota":{"collected_at":"2026-09-11T12:05:00Z","balances":[{"kind":"","currency":"USD","remaining":"2.50"},{"kind":"","currency":"CNY","remaining":"10"},{"kind":"","currency":"USD","remaining":"abc"}],"available":true},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"log_source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"log_source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"catalog":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"deepseek-v4","input":0.5,"output":1,"eligible":false}]},"conditional_remaining_token_estimates":[{"model":"deepseek-v4","currency":"USD","remaining":"2.50","tokens":250,"historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"]},{"model":"deepseek-x","currency":"USD","remaining":"2.50","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"historical_effective_rate_unavailable"},{"model":"deepseek-v4","currency":"CNY","remaining":"10","historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"unsupported_balance_currency"},{"model":"deepseek-x","currency":"CNY","remaining":"10","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"unsupported_balance_currency"},{"model":"deepseek-v4","currency":"USD","remaining":"abc","historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"invalid_balance_decimal"},{"model":"deepseek-x","currency":"USD","remaining":"abc","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"historical_effective_rate_unavailable"}]}],"unattributed_history":[{"log_source":"opencode","model":"mystery","provider":"unknown","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":7,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]}`
	wireFailedV2 = `{"schema_version":2,"generated_at":"2026-09-11T12:05:00Z","collection_started_at":"2026-09-11T12:05:00Z","collection_ended_at":"2026-09-11T12:05:00Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"error","last_attempt":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota","code":"rate_limited","retryable":true,"retry_at":"2026-09-11T12:09:00Z","message":"provider reading unavailable"},{"section":"history","code":"timeout","retryable":true,"message":"local usage history unavailable"},{"section":"reference_prices","code":"unavailable","retryable":true,"message":"reference prices unavailable"}],"calibration":{"unavailable_reason":"paired_quota_measurement_unavailable"}},{"provider":"codex","status":"error","last_attempt":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota","code":"credential_unavailable","retryable":false,"message":"report source unavailable"},{"section":"history","code":"timeout","retryable":true,"message":"local usage history unavailable"},{"section":"reference_prices","code":"unavailable","retryable":true,"message":"reference prices unavailable"}]},{"provider":"deepseek","status":"error","last_attempt":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota","code":"configuration_error","retryable":false,"message":"report source unavailable"},{"section":"history","code":"timeout","retryable":true,"message":"local usage history unavailable"},{"section":"reference_prices","code":"unavailable","retryable":true,"message":"reference prices unavailable"}]}]}`
	wireStaleV2  = `{"schema_version":2,"generated_at":"2026-09-11T12:05:00Z","collection_started_at":"2026-09-11T12:05:00Z","collection_ended_at":"2026-09-11T12:05:00Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":true,"age_seconds":3},"errors":[],"quota":{"collected_at":"2026-09-11T12:05:00Z","quotas":[{"id":"five_hour","bucket":"five_hour","kind":"session","used_percent":31.5,"unit":"percent_0_100","resets_at":"2026-09-11T13:05:00Z"}]},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"blocks":[],"issues":null},"reference_prices":{"catalog":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"claude-sonnet-5","input":2,"output":10,"cache_write":2.5,"eligible":true}],"assumptions":["cache_write_5m"]},"calibration":{"unavailable_reason":"cached_measurement_expired"},"configured_plan":{"label":"max","multiplier":5,"provenance":"configured"}},{"provider":"codex","status":"partial","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":true,"age_seconds":3},"errors":[{"section":"quota","code":"credential_unavailable","retryable":false,"message":"report source unavailable"}],"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"catalog":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"gpt-5.6-sol","input":4,"output":20,"cache_read":0.4,"eligible":true},{"model":"old-gpt","input":1,"output":2,"eligible":false}],"assumptions":["base_tier"]},"calibration":{"unavailable_reason":"cached_measurement_expired"}},{"provider":"deepseek","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":true,"age_seconds":3},"errors":[],"quota":{"collected_at":"2026-09-11T12:05:00Z","balances":[{"kind":"","currency":"USD","remaining":"2.50"},{"kind":"","currency":"CNY","remaining":"10"},{"kind":"","currency":"USD","remaining":"abc"}],"available":true},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"log_source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"log_source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"catalog":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"deepseek-v4","input":0.5,"output":1,"eligible":false}]},"calibration":{"unavailable_reason":"cached_measurement_expired"}}],"unattributed_history":[{"log_source":"opencode","model":"mystery","provider":"unknown","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":7,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]}`
	wireResetV2  = `{"schema_version":2,"generated_at":"2026-09-11T14:05:00Z","collection_started_at":"2026-09-11T12:05:00Z","collection_ended_at":"2026-09-11T12:05:00Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":true,"age_seconds":0},"errors":[],"quota":{"collected_at":"2026-09-11T12:05:00Z","quotas":[{"id":"five_hour","bucket":"five_hour","kind":"session","used_percent":31.5,"unit":"percent_0_100","resets_at":"2026-09-11T13:05:00Z"}]},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[]},"blocks":[],"issues":null},"reference_prices":{"catalog":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"claude-sonnet-5","input":2,"output":10,"cache_write":2.5,"eligible":true}],"assumptions":["cache_write_5m"]},"calibration":{"unavailable_reason":"quota_window_reset_after_collection"},"configured_plan":{"label":"max","multiplier":5,"provenance":"configured"}},{"provider":"codex","status":"partial","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[{"section":"quota","code":"credential_unavailable","retryable":false,"message":"report source unavailable"}],"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"old-gpt","provider":"codex","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":10,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"catalog":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"gpt-5.6-sol","input":4,"output":20,"cache_read":0.4,"eligible":true},{"model":"old-gpt","input":1,"output":2,"eligible":false}],"assumptions":["base_tier"]}},{"provider":"deepseek","status":"ok","last_attempt":"2026-09-11T12:05:00Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":false,"stale":false,"age_seconds":0},"errors":[],"quota":{"collected_at":"2026-09-11T12:05:00Z","balances":[{"kind":"","currency":"USD","remaining":"2.50"},{"kind":"","currency":"CNY","remaining":"10"},{"kind":"","currency":"USD","remaining":"abc"}],"available":true},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"log_source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01},{"log_source":"claude","model":"deepseek-x","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":5,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]},"blocks":[],"issues":null},"reference_prices":{"catalog":"models.dev","observed_at":"2026-09-11T12:05:00Z","unit":"usd_per_million_tokens","models":[{"model":"deepseek-v4","input":0.5,"output":1,"eligible":false}]},"conditional_remaining_token_estimates":[{"model":"deepseek-v4","currency":"USD","remaining":"2.50","tokens":250,"historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"]},{"model":"deepseek-x","currency":"USD","remaining":"2.50","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"historical_effective_rate_unavailable"},{"model":"deepseek-v4","currency":"CNY","remaining":"10","historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"unsupported_balance_currency"},{"model":"deepseek-x","currency":"CNY","remaining":"10","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"unsupported_balance_currency"},{"model":"deepseek-v4","currency":"USD","remaining":"abc","historical_effective_usd_per_token":0.01,"assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"invalid_balance_decimal"},{"model":"deepseek-x","currency":"USD","remaining":"abc","assumptions":["historical_api_reference_rate_matches_future_workload","future_provider_prices_do_not_change"],"unavailable_reason":"historical_effective_rate_unavailable"}]}],"unattributed_history":[{"log_source":"opencode","model":"mystery","provider":"unknown","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":7,"cost_usd":null,"cost_status":"unavailable_or_unpriced","historical_effective_usd_per_token":null,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices"}]}`
	wireCachedV2 = `{"schema_version":2,"generated_at":"2026-09-11T12:05:02Z","collection_started_at":"2026-09-11T12:05:02Z","collection_ended_at":"2026-09-11T12:05:02Z","history_range":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z"},"providers":[{"provider":"anthropic","status":"error","last_attempt":"2026-09-11T12:05:02Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":false,"age_seconds":0},"errors":[{"section":"quota","code":"credential_unavailable","retryable":false,"message":"report source unavailable"},{"section":"history","code":"unavailable","retryable":false,"message":"report source unavailable"}],"calibration":{"unavailable_reason":"paired_quota_measurement_unavailable"}},{"provider":"codex","status":"error","last_attempt":"2026-09-11T12:05:02Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":false,"age_seconds":0},"errors":[{"section":"quota","code":"credential_unavailable","retryable":false,"message":"report source unavailable"},{"section":"history","code":"unavailable","retryable":false,"message":"report source unavailable"}]},{"provider":"deepseek","status":"error","last_attempt":"2026-09-11T12:05:02Z","last_success":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":false,"age_seconds":0},"errors":[{"section":"quota","code":"unavailable","retryable":false,"message":"report source unavailable"},{"section":"history","code":"unavailable","retryable":false,"message":"report source unavailable"}],"last_complete_snapshot":{"measured_at":"2026-09-11T12:05:00Z","source_freshness":{"cached":true,"stale":true,"age_seconds":2},"quota":{"collected_at":"2026-09-11T12:05:00Z"},"history":{"started_at":"2026-09-11T12:05:00Z","finished_at":"2026-09-11T12:05:00Z","collector":"ccusage","coverage":"local_only","cost_basis":"calculated_api_reference_usd","tool_version":"20.0.20","invocation_mode":"native","unit_prices_available":false,"unit_price_unavailable_reason":"ccusage_does_not_report_unit_prices","seven_days":{"since":"2026-09-05T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01}]},"thirty_days":{"since":"2026-08-13T00:00:00Z","until":"2026-09-11T00:00:00Z","models":[{"log_source":"claude","model":"deepseek-v4","provider":"deepseek","input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"total_tokens":100,"cost_usd":1,"cost_status":"available","historical_effective_usd_per_token":0.01}]},"blocks":[],"issues":null},"calibration":{"unavailable_reason":"cached_measurement_expired"}}}]}`
)

// wireCachedBodiesV2 mirrors wire_test.go's wireCachedBody through the
// schema-2 handler: two collections, the second with DeepSeek and history
// failing, so the second body carries a last_complete_snapshot and the
// memory-backed last_success outlives the failed attempt. Each collection is
// requested at schema 1 first and then at schema 2, so the schema-2 bodies
// are cache hits on the collection schema 1 triggered (source_freshness
// .cached is true) and the test can check both came from one collection.
func wireCachedBodiesV2(t *testing.T, now time.Time) (v1, v2 []byte, collections int64) {
	t.Helper()
	failing := false
	var calls atomic.Int64
	h := New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			calls.Add(1)
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
	request := func(handler http.Handler) []byte {
		r := httptest.NewRequest(http.MethodGet, "/utraque/providers/v2", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	request(h)
	request(h.V2())
	now = now.Add(2 * time.Second)
	failing = true
	v1 = request(h)
	v2 = request(h.V2())
	return v1, v2, calls.Load()
}

func TestReportV2WireBytes(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	mixed := wireMixedReport(t, now)
	stale := wireMixedReport(t, now)
	markFreshness(&stale, true, true, 3*time.Second)
	reset := wireMixedReport(t, now)
	reset.GeneratedAt = now.Add(2 * time.Hour)
	markFreshness(&reset, false, false, 0)
	_, cached, _ := wireCachedBodiesV2(t, now)
	for _, tc := range []struct {
		name string
		want string
		got  any
	}{
		{"mixed", wireMixedV2, renderV2(mixed, nil)},
		{"failed", wireFailedV2, renderV2(wireFailedReport(t, now), nil)},
		{"stale", wireStaleV2, renderV2(stale, nil)},
		{"reset", wireResetV2, renderV2(reset, nil)},
		{"cached", wireCachedV2, json.RawMessage(cached)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.got)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tc.want {
				t.Fatalf("wire bytes changed\n got: %s\nwant: %s", body, tc.want)
			}
			for _, gone := range []string{`"quota_after"`, `"quota_before"`, `"paired_measurement"`, `"scope_id"`, `"extra_usage"`, `"spend_controls"`, `"balance"`, `"total"`, `"components"`, `"section":"quota_after"`} {
				if strings.Contains(string(body), gone) {
					t.Errorf("schema-1 key %s present in schema 2", gone)
				}
			}
			// "source" survives only on history.blocks[], usagehistory's own
			// rows; the fixtures have no blocks, so it must not appear at all.
			if strings.Contains(string(body), `"source"`) {
				t.Errorf(`"source" present in schema 2: %s`, body)
			}
			if !strings.Contains(string(body), `"schema_version":2`) {
				t.Errorf("schema_version missing")
			}
		})
	}
}

// TestSchemasServedFromOneCollection checks that a schema-1 and a schema-2
// request share one collection and one cache entry, and that the schema-2
// document's cross-attempt memory is what schema 1 cannot express: after the
// failed second attempt, schema 1 drops last_success while schema 2 keeps the
// first attempt's, and the retained snapshot says when it was measured.
func TestSchemasServedFromOneCollection(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)
	v1Body, v2Body, collections := wireCachedBodiesV2(t, now)
	if collections != 2 {
		t.Fatalf("history collections=%d want 2 (one per attempt, shared by both schemas)", collections)
	}
	var v1 Report
	var v2 ReportV2
	if err := json.Unmarshal(v1Body, &v1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(v2Body, &v2); err != nil {
		t.Fatal(err)
	}
	if v1.SchemaVersion != 1 || v2.SchemaVersion != 2 {
		t.Fatalf("schema versions %d/%d", v1.SchemaVersion, v2.SchemaVersion)
	}
	if !v1.CollectionEndedAt.Equal(v2.CollectionEndedAt) || !v1.GeneratedAt.Equal(v2.GeneratedAt) {
		t.Fatalf("documents describe different collections: %v vs %v", v1.CollectionEndedAt, v2.CollectionEndedAt)
	}
	p1 := providerNamed(v1, leg.DeepSeek)
	var p2 *ProviderReportV2
	for i := range v2.Providers {
		if v2.Providers[i].Provider == leg.DeepSeek {
			p2 = &v2.Providers[i]
		}
	}
	if p1 == nil || p2 == nil || p1.Status != StatusError || p2.Status != StatusError {
		t.Fatalf("deepseek sections v1=%+v v2=%+v", p1, p2)
	}
	if p1.LastSuccess != nil {
		t.Fatalf("schema 1 last_success is per attempt and must be omitted on a failed attempt: %v", p1.LastSuccess)
	}
	if p2.LastSuccess == nil || !p2.LastSuccess.Equal(now) {
		t.Fatalf("schema 2 last_success=%v want the first attempt %v", p2.LastSuccess, now)
	}
	if !p2.LastAttempt.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("schema 2 last_attempt=%v want the second attempt", p2.LastAttempt)
	}
	if p2.LastComplete == nil || p2.LastComplete.MeasuredAt == nil || !p2.LastComplete.MeasuredAt.Equal(now) {
		t.Fatalf("schema 2 last_complete_snapshot=%+v want measured_at %v", p2.LastComplete, now)
	}
	if p1.LastComplete == nil || p1.LastComplete.LastSuccess == nil || !p1.LastComplete.LastSuccess.Equal(*p2.LastComplete.MeasuredAt) {
		t.Fatalf("schema 1 snapshot last_success %+v differs from schema 2 measured_at", p1.LastComplete)
	}
	if p2.LastComplete.Quota == nil || !p2.LastComplete.Quota.CollectedAt.Equal(p1.LastComplete.Quota.CollectedAt) {
		t.Fatalf("snapshot observations differ: %+v vs %+v", p2.LastComplete.Quota, p1.LastComplete.Quota)
	}
	for _, e := range p2.Errors {
		if e.Section == SectionQuota {
			t.Fatalf("schema 2 error carries the schema-1 section: %+v", e)
		}
	}
}

// TestMemoryFollowsAttemptsAndProviderNormaliserOutput drives three attempts
// through the schema-2 handler with observations shaped exactly as the
// providerquota normalisers build them (Bucket, typed kinds, SpendLimits, and
// the schema-1 fan-out rows alongside) and checks the schema-2 quota reading
// and the last_* memory attempt by attempt.
func TestMemoryFollowsAttemptsAndProviderNormaliserOutput(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	weekly := now.Add(7 * 24 * time.Hour)
	active, inactive, reached, enabled, hasCredits, unlimited := true, false, false, true, true, false
	limit, used, monthly, usedCredits := "50.00", "12.25", "100", "25.5"
	spendPct, extraPct := 24.5, 25.5
	fiveHours, sevenDays := int64(5*60*60), int64(7*24*60*60)
	codexObs := providerquota.Observation{Source: leg.Codex, CollectedAt: now,
		Quotas: []providerquota.Quota{
			{ID: "codex", Bucket: "codex", Name: "Codex", Slot: "primary", UsedPercent: 25, Unit: "percent_0_100", DurationSeconds: &fiveHours, ResetsAt: &reset, Plan: &providerquota.PlanInfo{Type: "plus"}, ReachedType: "none"},
			{ID: "codex", Bucket: "codex", Name: "Codex", Slot: "secondary", UsedPercent: 40, Unit: "percent_0_100", DurationSeconds: &sevenDays, ResetsAt: &weekly, Plan: &providerquota.PlanInfo{Type: "plus"}, ReachedType: "none"},
			{ID: "codex:spend_control", Bucket: "codex", Kind: providerquota.QuotaKindSpendControl, UsedPercent: spendPct, Unit: "percent_0_100", ResetsAt: &weekly},
		},
		Balances: []providerquota.Balance{
			{Kind: providerquota.BalanceKindWorkspaceCredits, LimitID: "codex", AmountUnit: "credits", Total: "12.50", Available: &hasCredits, Unlimited: &unlimited},
			{Kind: providerquota.BalanceKindSpendControl, LimitID: "codex", AmountUnit: "provider_units", Total: limit, Components: []providerquota.BalanceComponent{{Name: "used", Amount: used}}},
		},
		SpendControls: []providerquota.SpendControl{{LimitID: "codex", Reached: reached}},
		SpendLimits:   []providerquota.SpendLimit{{LimitID: "codex", Limit: &limit, Used: &used, AmountUnit: "provider_units", UsedPercent: &spendPct, Unit: "percent_0_100", ResetsAt: &weekly, Reached: &reached}},
		Plan:          &providerquota.PlanInfo{Type: "plus"},
		ResetCredits:  &providerquota.ResetCredits{AvailableCount: 2}}
	scope := &providerquota.Scope{Model: &providerquota.ScopeLabel{ID: "claude-opus-5", DisplayName: "Opus"}, Surface: &providerquota.ScopeLabel{ID: "code", DisplayName: "Code"}}
	anthropicObs := providerquota.Observation{Source: leg.Anthropic, CollectedAt: now,
		Quotas: []providerquota.Quota{
			{ID: "session", Bucket: "session", Kind: "session", Group: "session", UsedPercent: 4, Unit: "percent_0_100", DurationSeconds: &fiveHours, ResetsAt: &reset, Active: &active},
			{ID: "weekly_all:model=claude-opus-5:surface=code", Bucket: "weekly_all", Kind: "weekly_all", Group: "weekly", UsedPercent: 50, Unit: "percent_0_100", DurationSeconds: &sevenDays, ResetsAt: &weekly, Scope: scope, Active: &inactive},
			{ID: "cinder_cove", Bucket: "cinder_cove", Kind: "cinder_cove", UsedPercent: 3, Unit: "percent_0_100"},
		},
		SpendLimits: []providerquota.SpendLimit{{Enabled: &enabled, Limit: &monthly, Used: &usedCredits, AmountUnit: "provider_units", Currency: "USD", UsedPercent: &extraPct, Unit: "percent_0_100"}},
		ExtraUsage:  &providerquota.ExtraUsage{Enabled: enabled, MonthlyLimit: &monthly, UsedCredits: &usedCredits, AmountUnit: "provider_units", UsedPercent: &extraPct, Unit: "percent_0_100", Currency: "USD"}}
	attempt := 0
	h := New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			return sampleHistory(now), nil
		}),
		Codex: codexFunc(func(context.Context, auth.CredentialSource, auth.Credential) (providerquota.Observation, error) {
			if attempt == 2 {
				return providerquota.Observation{}, &providerquota.Error{Provider: leg.Codex, Code: providerquota.CodeUnavailable, Retryable: true}
			}
			return codexObs, nil
		}),
		CodexSource: sourceFunc(func(context.Context) (auth.Credential, error) {
			return auth.Credential{AccessToken: "codex-secret", AccountID: "account"}, nil
		}),
		Anthropic: anthropicFunc(func(context.Context, string) (providerquota.Observation, error) {
			if attempt >= 2 {
				return providerquota.Observation{}, &providerquota.Error{Provider: leg.Anthropic, Code: providerquota.CodeRateLimited, Retryable: true, RetryAt: &reset, AttemptedAt: now.Add(-time.Minute)}
			}
			return anthropicObs, nil
		}),
		CacheTTL: time.Second, Now: func() time.Time { return now }})
	request := func() (ReportV2, string) {
		attempt++
		r := httptest.NewRequest(http.MethodGet, "/utraque/providers/v2", nil)
		r.RemoteAddr = "127.0.0.1:4000"
		r.Header.Set("Authorization", "Bearer anthropic-secret")
		w := httptest.NewRecorder()
		h.V2().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var out ReportV2
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out, w.Body.String()
	}
	provider := func(r ReportV2, id leg.ID) *ProviderReportV2 {
		for i := range r.Providers {
			if r.Providers[i].Provider == id {
				return &r.Providers[i]
			}
		}
		t.Fatalf("no %s section", id)
		return nil
	}

	// Attempt 1: everything succeeds. The quota readings are the schema-2
	// projections of the normaliser output.
	first, body := request()
	codex := provider(first, leg.Codex)
	if codex.Status != StatusOK || codex.Quota == nil {
		t.Fatalf("codex=%+v", codex)
	}
	wantCodexQuota := `{"collected_at":"2026-09-11T12:00:00Z","quotas":[{"id":"codex:primary","bucket":"codex","kind":"session","name":"Codex","slot":"primary","used_percent":25,"unit":"percent_0_100","duration_seconds":18000,"resets_at":"2026-09-11T13:00:00Z","plan":{"type":"plus"},"reached_type":"none"},{"id":"codex:secondary","bucket":"codex","kind":"weekly","name":"Codex","slot":"secondary","used_percent":40,"unit":"percent_0_100","duration_seconds":604800,"resets_at":"2026-09-18T12:00:00Z","plan":{"type":"plus"},"reached_type":"none"}],"balances":[{"kind":"workspace_credits","limit_id":"codex","amount_unit":"credits","remaining":"12.50","available":true,"unlimited":false}],"spend_limits":[{"limit_id":"codex","limit":"50.00","used":"12.25","amount_unit":"provider_units","used_percent":24.5,"unit":"percent_0_100","resets_at":"2026-09-18T12:00:00Z","reached":false}],"plan":{"type":"plus"},"reset_credits":{"available_count":2}}`
	if got, _ := json.Marshal(codex.Quota); string(got) != wantCodexQuota {
		t.Fatalf("codex quota reading\n got: %s\nwant: %s", got, wantCodexQuota)
	}
	anthropic := provider(first, leg.Anthropic)
	wantAnthropicQuota := `{"collected_at":"2026-09-11T12:00:00Z","quotas":[{"id":"session","bucket":"session","kind":"session","group":"session","used_percent":4,"unit":"percent_0_100","duration_seconds":18000,"resets_at":"2026-09-11T13:00:00Z","active":true},{"id":"weekly_all:model=claude-opus-5:surface=code","bucket":"weekly_all","kind":"weekly_scoped","group":"weekly","used_percent":50,"unit":"percent_0_100","duration_seconds":604800,"resets_at":"2026-09-18T12:00:00Z","scope":{"model":{"id":"claude-opus-5","display_name":"Opus"},"surface":{"id":"code","display_name":"Code"}},"active":false},{"id":"cinder_cove","bucket":"cinder_cove","kind":"other","used_percent":3,"unit":"percent_0_100"}],"spend_limits":[{"enabled":true,"limit":"100","used":"25.5","amount_unit":"provider_units","currency":"USD","used_percent":25.5,"unit":"percent_0_100"}]}`
	if got, _ := json.Marshal(anthropic.Quota); string(got) != wantAnthropicQuota {
		t.Fatalf("anthropic quota reading\n got: %s\nwant: %s", got, wantAnthropicQuota)
	}
	for _, secret := range []string{"anthropic-secret", "codex-secret", "account"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked %q", secret)
		}
	}
	if codex.LastSuccess == nil || !codex.LastSuccess.Equal(now) || !codex.LastAttempt.Equal(now) {
		t.Fatalf("attempt 1 memory: %+v", codex)
	}

	// Attempt 2: Codex fails and Anthropic is rate-limited with an earlier
	// real attempt. Codex keeps attempt 1's last_success; its last_attempt is
	// this collection's end and never the quota leg's attempted_at, which
	// stays on the error.
	now = now.Add(2 * time.Second)
	second, _ := request()
	codex = provider(second, leg.Codex)
	if codex.Status != StatusPartial || codex.Quota != nil || codex.LastSuccess == nil || !codex.LastSuccess.Equal(now) {
		t.Fatalf("attempt 2 codex (partial: history succeeded): %+v", codex)
	}
	anthropic = provider(second, leg.Anthropic)
	if anthropic.Status != StatusPartial || !anthropic.LastAttempt.Equal(now) || len(anthropic.Errors) != 1 || anthropic.Errors[0].Section != SectionQuotaV2 || anthropic.Errors[0].AttemptedAt == nil || !anthropic.Errors[0].AttemptedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("attempt 2 anthropic: %+v errors=%+v", anthropic, anthropic.Errors)
	}
	if anthropic.LastComplete == nil || anthropic.LastComplete.MeasuredAt == nil || !anthropic.LastComplete.MeasuredAt.Equal(now.Add(-2*time.Second)) || anthropic.LastComplete.Quota == nil || len(anthropic.LastComplete.Quota.SpendLimits) != 1 || anthropic.LastComplete.Quota.Quotas[1].ID != "weekly_all:model=claude-opus-5:surface=code" {
		t.Fatalf("attempt 2 anthropic snapshot lost the schema-2 fields through the cache: %+v", anthropic.LastComplete)
	}

	// Attempt 3: served from the cache (within TTL) — the memory is the
	// entry's, not recomputed.
	now = now.Add(500 * time.Millisecond)
	third, _ := request()
	if a := provider(third, leg.Anthropic); !a.LastAttempt.Equal(now.Add(-500*time.Millisecond)) || !a.SourceFreshness.Cached {
		t.Fatalf("attempt 3 (cached): %+v", a)
	}
}
