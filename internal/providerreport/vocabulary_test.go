package providerreport

import (
	"os"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/referenceprice"
	"github.com/hughescr/utraque/internal/usagehistory"
)

// reportVocabularies lists every closed value set the schema-2 document
// serves, from the typed constants that are its source of truth. The README's
// "Provider report" section must enumerate each value; the test below fails
// when the two drift, so the constants and the documentation cannot disagree.
var reportVocabularies = map[string][]string{
	"status":  {string(StatusOK), string(StatusPartial), string(StatusError)},
	"section": {string(SectionQuotaV2), string(SectionHistory), string(SectionReferencePrices)},
	"code": {
		// Raised by this package.
		string(CodeUnavailable), string(CodeCredentialUnavailable), string(CodeConfigurationError),
		string(CodeAccountScopeChanged), string(CodeAccountScopeUnverified),
		// Carried unchanged from the quota readers.
		string(providerquota.CodeConfiguration), string(providerquota.CodeCredential), string(providerquota.CodeUnauthorized),
		string(providerquota.CodeRateLimited), string(providerquota.CodeUnavailable), string(providerquota.CodeInvalidData),
		string(providerquota.CodeTooLarge), string(providerquota.CodeTimeout), string(providerquota.CodeProtocol),
		// Carried unchanged from the history collector.
		string(usagehistory.ErrorInvalidRequest), string(usagehistory.ErrorCommand), string(usagehistory.ErrorTimeout),
		string(usagehistory.ErrorCanceled), string(usagehistory.ErrorOutputLimit), string(usagehistory.ErrorIncompatible),
		// Carried unchanged from the reference-price reader.
		string(referenceprice.CodeConfiguration), string(referenceprice.CodeUnavailable), string(referenceprice.CodeTimeout),
		string(referenceprice.CodeTooLarge), string(referenceprice.CodeInvalidData),
	},
	"unavailable_reason": {
		string(ReasonPairedMeasurementUnavailable), string(ReasonCachedMeasurementExpired), string(ReasonQuotaWindowReset),
		string(ReasonUnsupportedBalanceCurrency), string(ReasonHistoricalRateUnavailable), string(ReasonInvalidBalanceDecimal),
	},
	"quotas[].kind": {
		string(providerquota.WindowSession), string(providerquota.WindowWeekly), string(providerquota.WindowWeeklyScoped), string(providerquota.WindowOther),
	},
	"balances[].kind": {
		string(providerquota.BalanceKindAccount), string(providerquota.BalanceKindWorkspaceCredits),
	},
}

// readmeProviderReportSection returns the README's "## Provider report"
// section up to the next H2.
func readmeProviderReportSection(t *testing.T) string {
	t.Helper()
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	const heading = "\n## Provider report\n"
	start := strings.Index(string(readme), heading)
	if start < 0 {
		t.Fatalf("README has no %q section", strings.TrimSpace(heading))
	}
	section := string(readme)[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	return section
}

func TestREADMEEnumeratesReportVocabularies(t *testing.T) {
	section := readmeProviderReportSection(t)
	for field, values := range reportVocabularies {
		for _, value := range values {
			if !strings.Contains(section, "`"+value+"`") {
				t.Errorf("README's Provider report section does not list %s value `%s`", field, value)
			}
		}
	}
	for _, path := range []string{"/utraque/providers/v2", "/utraque/providers/v1", "/v1/utraque/providers"} {
		if !strings.Contains(section, "`GET "+path+"`") {
			t.Errorf("README's Provider report section does not document `GET %s`", path)
		}
	}
}
