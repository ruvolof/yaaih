package browser

import (
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

func TestParseChromiumMajor(t *testing.T) {
	cases := []struct {
		name   string
		banner string
		want   string
	}{
		{"chromium banner", "Chromium 141.0.7390.54", "141"},
		{"chrome for testing banner", "Google Chrome for Testing 133.0.6943.98", "133"},
		{"surrounding whitespace", "\n  Chromium 120.0.6099.109  \n", "120"},
		{"probe returned nothing", "", ""},
		{"binary missing", "sh: 1: chrome: not found", ""},
		{"no version component", "Chromium", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseChromiumMajor(tc.banner); got != tc.want {
				t.Errorf("parseChromiumMajor(%q) = %q, want %q", tc.banner, got, tc.want)
			}
		})
	}
}

func TestDesktopUserAgentNeverAdvertisesHeadless(t *testing.T) {
	for _, major := range []string{"141", ""} {
		userAgent := desktopUserAgent(major)
		if strings.Contains(userAgent, "Headless") {
			t.Errorf("desktopUserAgent(%q) = %q, must not contain Headless", major, userAgent)
		}
		if !strings.Contains(userAgent, "Chrome/") {
			t.Errorf("desktopUserAgent(%q) = %q, want a Chrome/ token", major, userAgent)
		}
	}
	if withVersion := desktopUserAgent("141"); !strings.Contains(withVersion, "Chrome/141.0.0.0") {
		t.Errorf("desktopUserAgent(\"141\") = %q, want the probed major version in it", withVersion)
	}
	if fallback := desktopUserAgent(""); !strings.Contains(fallback, "Chrome/"+fallbackChromiumMajor+".") {
		t.Errorf("desktopUserAgent(\"\") = %q, want the fallback major version in it", fallback)
	}
}

func TestWaitForSelectorState(t *testing.T) {
	cases := []struct {
		state string
		want  *playwright.WaitForSelectorState
	}{
		{"", playwright.WaitForSelectorStateVisible},
		{"visible", playwright.WaitForSelectorStateVisible},
		{"hidden", playwright.WaitForSelectorStateHidden},
		{"attached", playwright.WaitForSelectorStateAttached},
		{"detached", playwright.WaitForSelectorStateDetached},
	}
	for _, tc := range cases {
		got, err := waitForSelectorState(tc.state)
		if err != nil {
			t.Fatalf("waitForSelectorState(%q): unexpected error: %v", tc.state, err)
		}
		if *got != *tc.want {
			t.Errorf("waitForSelectorState(%q) = %q, want %q", tc.state, *got, *tc.want)
		}
	}

	if _, err := waitForSelectorState("loaded"); err == nil {
		t.Error("waitForSelectorState(\"loaded\"): want an error, got nil")
	}
}
