package sse

import "errors"

// errLineTooLong is returned by Scan when a single logical line exceeds the
// configured maximum, guarding against an unbounded buffer on a corrupt or
// hostile stream.
var errLineTooLong = errors.New("utraque/sse: line exceeds maximum length")

// ErrLineTooLong exposes the sentinel for errors.Is checks.
var ErrLineTooLong = errLineTooLong
