package tool

import (
	"testing"
	"unicode/utf8"
)

// FuzzCheckWebURL asserts that URL validation never panics on arbitrary string inputs.
func FuzzCheckWebURL(f *testing.F) {
	seeds := []string{
		"http://example.com",
		"https://example.com/path?query=1#hash",
		"http://user:pass@example.com",
		"ftp://example.com",
		"http://127.0.0.1:8080",
		"http://[::1]/test",
		"https://169.254.169.254/latest/meta-data",
		"javascript:alert(1)",
		"data:text/html,<html>",
		"",
		"   ",
		"https://",
		"http://foo bar",
		"https://example.com:notaport",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		u, err := checkWebURL(raw)
		if err == nil {
			if u == nil {
				t.Fatal("nil error but nil url")
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				t.Fatalf("accepted non-http scheme: %q", u.Scheme)
			}
			if u.Host == "" {
				t.Fatal("accepted empty host")
			}
			if u.User != nil {
				t.Fatal("accepted user info in url")
			}
		}
	})
}

// FuzzHTMLText asserts that htmlText reduction never panics on arbitrary HTML input.
func FuzzHTMLText(f *testing.F) {
	seeds := []string{
		"<html><head><title>Test</title></head><body><h1>Hello</h1><p>World</p></body></html>",
		"<script>alert('xss');</script><p>Safe text</p>",
		"<!-- comment with <script> --> visible",
		"<div>Nested <span>elements <b>with</b></span> formatting</div>",
		"<title>Multi\nLine\nTitle</title>",
		"<a href='http://example.com'>Link</a>",
		"<style>body { color: red; }</style>",
		"<script>unclosed tag",
		"<<<<>>>>>",
		"",
		"Plain text with no markup",
		"Entities: &amp; &lt; &gt; &#39; &quot;",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		got := htmlText(input)
		if !utf8.ValidString(got) {
			t.Fatal("htmlText produced invalid UTF-8 string")
		}
	})
}
