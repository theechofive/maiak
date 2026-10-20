//go:build maiaktest

package safehttp

import (
	"crypto/x509"
	"net/http"
)

// NewLoopbackTestClient returns the guarded client of NewClient with exactly two test-only
// relaxations: loopback addresses (127.0.0.0/8, ::1) pass the address validation, and every
// validated address is dialed on port instead of 443; roots replaces the system CAs. All other
// rules stay active: https only, URL port 443, redirect limits and re-validation, every other
// blocked range, the connect-time Control check, no proxy. Map hostnames to loopback with
// opts.Resolver.
//
// This file is compiled only with `-tags maiaktest` (make test-e2e, L-030, L-041); release
// builds never set the tag, so the symbol does not exist there. TestOverrideNotSetOutsideTests
// enforces that this is the only tagged file and the only place setting these options.
func NewLoopbackTestClient(opts Options, roots *x509.CertPool, port uint16) *http.Client {
	if port == 0 {
		panic("safehttp: NewLoopbackTestClient needs a port")
	}
	opts.loopbackPort = port
	opts.rootCAs = roots
	return NewClient(opts)
}
