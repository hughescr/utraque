package apierr_test

import (
	"net/http"
	"testing"

	"github.com/hughescr/utraque/internal/apierr"
)

// TestTaxonomyMatchesPublishedStatusCodes pins the status<->type mapping to
// the table at https://platform.claude.com/docs/en/api/errors, in both
// directions, so a 402 or 409 relayed from upstream renders as the type a
// client of api.anthropic.com would have seen rather than as api_error.
func TestTaxonomyMatchesPublishedStatusCodes(t *testing.T) {
	cases := []struct {
		status  int
		errType apierr.ErrorType
	}{
		{http.StatusBadRequest, apierr.TypeInvalidRequest},
		{http.StatusUnauthorized, apierr.TypeAuthentication},
		{http.StatusPaymentRequired, apierr.TypeBilling},
		{http.StatusForbidden, apierr.TypePermission},
		{http.StatusNotFound, apierr.TypeNotFound},
		{http.StatusConflict, apierr.TypeConflict},
		{http.StatusRequestEntityTooLarge, apierr.TypeRequestTooLarge},
		{http.StatusTooManyRequests, apierr.TypeRateLimit},
		{http.StatusInternalServerError, apierr.TypeAPI},
		{http.StatusGatewayTimeout, apierr.TypeTimeout},
		{529, apierr.TypeOverloaded},
	}
	for _, tc := range cases {
		if got := apierr.TypeForStatus(tc.status); got != tc.errType {
			t.Errorf("TypeForStatus(%d) = %q, want %q", tc.status, got, tc.errType)
		}
		if got := apierr.StatusFor(tc.errType); got != tc.status {
			t.Errorf("StatusFor(%q) = %d, want %d", tc.errType, got, tc.status)
		}
	}
}

func TestBillingAndConflictWireValues(t *testing.T) {
	if apierr.TypeBilling != "billing_error" || apierr.TypeConflict != "conflict_error" {
		t.Fatalf("billing=%q conflict=%q, want billing_error / conflict_error", apierr.TypeBilling, apierr.TypeConflict)
	}
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusPaymentRequired, "billing_error"},
		{http.StatusConflict, "conflict_error"},
	} {
		e := apierr.New(apierr.TypeForStatus(tc.status), "upstream said no")
		if e.HTTPStatus() != tc.status {
			t.Errorf("HTTPStatus for %d-derived error = %d", tc.status, e.HTTPStatus())
		}
		if env := e.Envelope(); env.Error.Type != tc.want {
			t.Errorf("envelope type for %d = %q, want %q", tc.status, env.Error.Type, tc.want)
		}
	}
}
