package main

import "testing"

// A released binary keeps its data in one place wherever it is started; only
// a development build takes the checkout it runs in as its home.
func TestOnlyADevBuildUsesTheCheckoutAsHome(t *testing.T) {
	found := func() (string, bool) { return "/src/ariadne", true }
	none := func() (string, bool) { return "", false }

	if got := checkoutHome("dev", found); got != "/src/ariadne" {
		t.Errorf("dev build in a checkout: home %q, want the checkout", got)
	}
	if got := checkoutHome("dev", none); got != "" {
		t.Errorf("dev build outside a checkout: home %q, want the user directory", got)
	}
	for _, v := range []string{"v0.6.8", "v0.6.8-rc1"} {
		if got := checkoutHome(v, found); got != "" {
			t.Errorf("%s started inside a checkout: home %q, want the user directory", v, got)
		}
	}
}
