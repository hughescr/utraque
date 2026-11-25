package catalog_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/codex/catalog"
)

type fixtureRoundTrip func(*http.Request) (*http.Response, error)

func (f fixtureRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// http.Client normally discards the response if it cannot parse Location,
// before it even calls CheckRedirect. The catalog does not need a target.
func TestCatalogMalformedRedirectRetainsStatus(t *testing.T) {
	c := catalog.New(catalog.Options{HTTPClient: &http.Client{Transport: fixtureRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 308, Header: http.Header{"Location": {"/%zz"}}, Body: http.NoBody}, nil
	})}})
	_, err := c.Models(t.Context(), fakeCred())
	var failure *catalog.Failure
	if !errors.As(err, &failure) || failure.Code() != "redirect_refused" || failure.Status() != 308 {
		t.Fatalf("malformed redirect misclassified: %v", err)
	}
}

func TestCatalogFailureClassification(t *testing.T) {
	const marker = "CauseFixture84MkL"
	for _, tc := range []struct {
		code   string
		status int
		cause  error
		client int
	}{
		{"bad_request", 400, nil, 400}, {"credential_rejected", 401, nil, 401},
		{"redirect_refused", 302, nil, 502}, {"upstream_status", 503, nil, 503},
		{"unexpected_not_modified", 304, nil, 500}, {"decode", 200, nil, 500},
		{"transport", 0, errors.New(marker), 500}, {"timeout", 0, context.DeadlineExceeded, 504},
		{"canceled", 0, context.Canceled, 500},
	} {
		t.Run(tc.code, func(t *testing.T) {
			c := catalog.New(catalog.Options{HTTPClient: &http.Client{Transport: fixtureRoundTrip(func(r *http.Request) (*http.Response, error) {
				if tc.cause != nil {
					return nil, tc.cause
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": {"/signed?sig=" + marker}}, Body: io.NopCloser(strings.NewReader(marker))}, nil
			})}})
			_, err := c.Models(t.Context(), fakeCred())
			var f interface {
				Code() string
				Status() int
			}
			if !errors.As(err, &f) || f.Code() != tc.code || f.Status() != tc.status {
				t.Fatal("failure does not carry the expected reason and actual status")
			}
			ae := apierr.From(err)
			if strings.Contains(err.Error(), marker) || strings.Contains(ae.Message, marker) || ae.Err != nil || ae.HTTPStatus() != tc.client {
				t.Fatal("unsafe or incorrectly mapped client error")
			}
		})
	}
}
