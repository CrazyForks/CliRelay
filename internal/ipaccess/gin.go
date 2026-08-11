package ipaccess

import "github.com/gin-gonic/gin"

// GinContextKey is where the admission middleware stashes its findings so later
// layers reuse them instead of re-resolving the address. Re-resolving would not
// merely waste work: two layers deriving trust independently could disagree, and
// a throttle that thinks an address is trustworthy while admission does not is
// exactly the inconsistency that produces phantom lockouts.
const GinContextKey = "cliproxy.ipaccess"

// RequestContext is the per-request result of consulting the list.
type RequestContext struct {
	Address ClientAddress
	Verdict Verdict
}

// StoreContext publishes the findings for later middleware.
func StoreContext(c *gin.Context, rc RequestContext) {
	if c == nil {
		return
	}
	c.Set(GinContextKey, rc)
}

// FromGin reads back what the admission middleware recorded.
func FromGin(c *gin.Context) (RequestContext, bool) {
	if c == nil {
		return RequestContext{}, false
	}
	value, ok := c.Get(GinContextKey)
	if !ok {
		return RequestContext{}, false
	}
	rc, ok := value.(RequestContext)
	return rc, ok
}

// ExemptFromThrottle reports whether this request matched an allow rule and so
// must not be rate limited.
func ExemptFromThrottle(c *gin.Context) bool {
	rc, ok := FromGin(c)
	return ok && rc.Verdict.Exempt()
}

// AddressFor returns the resolved client address, falling back to resolving it
// on the spot for requests that never passed through the admission middleware
// (management routes mounted on a bare engine in tests, for instance).
func AddressFor(c *gin.Context) ClientAddress {
	if rc, ok := FromGin(c); ok {
		return rc.Address
	}
	if c == nil {
		return ClientAddress{}
	}
	return Default().ResolveAddress(c.Request, c.ClientIP())
}
