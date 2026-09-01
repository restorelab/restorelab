package cli

import "testing"

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"127.0.0.53:9999", true},
		{"0.0.0.0:8080", false},
		{":8080", false},
		{"192.0.2.10:8080", false},
		{"[::]:8080", false},
		{"example.internal:8080", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestAnAddressWithNoPortIsNotAssumedLocal(t *testing.T) {
	// Anything unparseable must fall on the safe side: treating it as public
	// means asking for a token, which is the answer that cannot hurt.
	if isLoopbackAddr("this is not an address") {
		t.Fatal("an unparseable address was treated as loopback")
	}
}
