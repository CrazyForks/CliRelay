package management

import "testing"

func TestMaskPortalIPHidesTheHostPart(t *testing.T) {
	// Enough for an end user to recognise "that was my office", not enough to
	// hand a stolen portal session a usable address.
	cases := []struct {
		in   string
		want string
	}{
		{"203.0.113.66", "203.0.113.×"},
		{"10.0.0.1", "10.0.0.×"},
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2:···"},
		{"", ""},
		{"not-an-ip", "not-an-ip"},
	}
	for _, tc := range cases {
		if got := maskPortalIP(tc.in); got != tc.want {
			t.Errorf("maskPortalIP(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMaskPortalIPKeepsShortIPv6Intact(t *testing.T) {
	// Too few groups to split meaningfully; returning it unchanged is better
	// than emitting something that looks masked but is not.
	if got := maskPortalIP("::1"); got != "::1" {
		t.Errorf("got %q, want the input unchanged", got)
	}
}
