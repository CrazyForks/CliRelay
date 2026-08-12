package ipaccess

import "context"

type addressContextKey struct{}

// WithAddress attaches the resolved client address to a request context.
//
// Audit records need the source address, and threading a parameter through every
// RecordAudit call site would be dozens of edits that a future call site would
// simply forget. The context carries it instead, so provenance is automatic.
func WithAddress(ctx context.Context, address ClientAddress) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, addressContextKey{}, address)
}

// AddressFromContext reads back the client address, if one was attached.
func AddressFromContext(ctx context.Context) (ClientAddress, bool) {
	if ctx == nil {
		return ClientAddress{}, false
	}
	address, ok := ctx.Value(addressContextKey{}).(ClientAddress)
	return address, ok
}

// AuditAddress renders the source address for an audit row.
//
// An untrusted address is recorded with a marker rather than silently: writing
// the proxy's address as if it were the actor's would make every audit row agree
// on a single wrong answer, which is worse than an honest blank.
func AuditAddress(ctx context.Context) string {
	address, ok := AddressFromContext(ctx)
	if !ok || address.Raw == "" {
		return ""
	}
	if !address.Trusted {
		return address.Raw + " (untrusted)"
	}
	return address.Raw
}
