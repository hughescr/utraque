package catalog

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"

	"github.com/hughescr/utraque/internal/apierr"
)

// Failure is safe to retain and publish. Neither field can hold a response
// body, redirect target, request URL, decoder input or transport error chain.
// A zero status means no HTTP response was available to the catalog.
type Failure struct {
	reason reason
	status int
}

type reason string

const (
	badRequest            reason = "bad_request"
	credentialRejected    reason = "credential_rejected"
	redirectRefused       reason = "redirect_refused"
	upstreamStatus        reason = "upstream_status"
	unexpectedNotModified reason = "unexpected_not_modified"
	transportFailure      reason = "transport"
	timeout               reason = "timeout"
	canceled              reason = "canceled"
	credentialAcquisition reason = "credential_acquisition_failed"
	noCredential          reason = "no_credential"
	decode                reason = "decode"
	invalidRequest        reason = "invalid_request"
)

// NoCredential classifies a local credential gate that did not permit a fetch.
func NoCredential() *Failure { return &Failure{reason: noCredential} }

// CredentialAcquisitionFailed means the gate passed but Get failed before a
// models request was issued. There is no catalog HTTP status to report.
func CredentialAcquisitionFailed() *Failure { return &Failure{reason: credentialAcquisition} }

func classifyIO(err error, status int) *Failure {
	if errors.Is(err, context.Canceled) {
		return &Failure{canceled, status}
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return &Failure{timeout, status}
	}
	return &Failure{transportFailure, status}
}

func (f *Failure) Code() string { return string(f.reason) }
func (f *Failure) Status() int  { return f.status }

// Error is the log projection; operators get the fixed explanation on health.
func (f *Failure) Error() string {
	if f.status != 0 {
		return f.Code() + " (HTTP " + strconv.Itoa(f.status) + ")"
	}
	return f.Code()
}

func (f *Failure) Message() string {
	switch f.reason {
	case badRequest:
		return "Codex rejected the catalog request. A missing or unsupported client_version is a likely cause; check UTRAQUE_CODEX_CLIENT_VERSION."
	case credentialRejected:
		return "Codex rejected the credential; the access token may need refreshing."
	case redirectRefused:
		return "Codex redirected the catalog request; redirects are not followed."
	case upstreamStatus:
		return "Codex returned an unexpected catalog status."
	case unexpectedNotModified:
		return "Codex returned not modified without a catalog validator."
	case timeout:
		return "The Codex catalog request timed out."
	case transportFailure:
		return "The Codex catalog request could not be completed."
	case canceled:
		return "The Codex catalog request was canceled."
	case noCredential:
		return "No usable Codex credential is available."
	case credentialAcquisition:
		return "The Codex credential could not be obtained for the catalog request."
	case decode:
		return "The Codex catalog response could not be decoded."
	case invalidRequest:
		return "The Codex catalog request URL is invalid."
	default:
		return "The Codex catalog request failed."
	}
}

// Unwrap preserves the client-facing API error type, not an upstream cause.
func (f *Failure) Unwrap() error {
	status := http.StatusInternalServerError
	switch f.reason {
	case badRequest, credentialRejected, upstreamStatus:
		if f.status >= 400 {
			status = f.status
		} else {
			status = http.StatusBadGateway
		}
	case redirectRefused:
		status = http.StatusBadGateway
	case timeout:
		status = http.StatusGatewayTimeout
	}
	return apierr.WithStatus(status, apierr.TypeForStatus(status), "%s", f.Message())
}
