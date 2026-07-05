package agent

import (
	"strings"
	"testing"
)

const prefix = "/api/clusters/abc123/proxy/longhorn"

// The shim must run before the app's own bundle, or the app can issue a request the wrappers never
// see. Injecting at the top of <head> is what guarantees that.
func TestInjectBasePathShimGoesFirstInHead(t *testing.T) {
	doc := `<!doctype html><html><head><meta charset="utf-8"><script src="/index.js"></script></head><body></body></html>`
	got := string(injectBasePathShim([]byte(doc), prefix))

	shimAt := strings.Index(got, "window.fetch=function")
	appAt := strings.Index(got, `src="/index.js"`)
	headAt := strings.Index(got, "<head>")
	if shimAt < 0 {
		t.Fatalf("no shim injected: %s", got)
	}
	if shimAt < headAt {
		t.Error("the shim landed outside <head>")
	}
	if shimAt > appAt {
		t.Error("the shim must precede the app's scripts, or a request could escape it")
	}
	if !strings.Contains(got, `var p="`+prefix+`"`) {
		t.Errorf("the shim does not carry the route prefix: %s", got)
	}
}

// A document with no <head> still has to get the shim first - the property that matters is ordering,
// not placement.
func TestInjectBasePathShimWithoutHead(t *testing.T) {
	got := string(injectBasePathShim([]byte(`<div>fragment</div>`), prefix))
	if !strings.HasPrefix(got, "<script>") {
		t.Fatalf("want the shim prepended, got %s", got)
	}
}

// Injecting twice would wrap the wrappers - harmless but pointless work on every request, and a sign
// the rewrite is running somewhere it shouldn't.
func TestInjectBasePathShimIsIdempotent(t *testing.T) {
	once := injectBasePathShim([]byte(`<html><head></head></html>`), prefix)
	twice := injectBasePathShim(once, prefix)
	if string(once) != string(twice) {
		t.Fatal("the shim was injected a second time")
	}
}

// The prefix arrives as an HTTP header, so it is untrusted input: it must not be able to break out of
// the JavaScript string it is placed in.
func TestInjectBasePathShimQuotesThePrefix(t *testing.T) {
	got := string(injectBasePathShim([]byte(`<html><head></head></html>`), `/x";alert(1);//`))
	// The escaped form is `"/x\";alert(1);//"` - what must NOT appear is the quote closing the
	// literal, i.e. the same text with no backslash in front of it.
	if strings.Contains(got, `"/x";`) {
		t.Fatalf("the prefix escaped its string literal: %s", got)
	}
	if !strings.Contains(got, `"/x\";alert(1);//"`) {
		t.Fatalf("want the quote escaped in place, got %s", got)
	}

	// A prefix carrying a closing tag would end the <script> ELEMENT, which no amount of JavaScript
	// quoting would prevent - encoding/json's default HTML escaping of < and > is what does.
	got = string(injectBasePathShim([]byte(`<html><head></head></html>`), `/x</script><img src=x onerror=alert(1)>`))
	if strings.Contains(got, "</script><img") {
		t.Fatalf("the prefix broke out of the script element: %s", got)
	}
}

func TestIsHTML(t *testing.T) {
	for _, tc := range []struct {
		ct   string
		want bool
	}{
		{"text/html; charset=utf-8", true},
		{"TEXT/HTML", true},
		{"text/css", false},
		{"application/javascript", false},
		{"", false},
	} {
		if got := isHTML(tc.ct); got != tc.want {
			t.Errorf("isHTML(%q) = %v, want %v", tc.ct, got, tc.want)
		}
	}
}

func TestHeadOpenEnd(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want int
	}{
		{"<html><head>", 12},
		{`<html><HEAD lang="en">`, 22},
		{"<html><body>", -1},
		{"<html><head", -1}, // unterminated
	} {
		if got := headOpenEnd([]byte(tc.doc)); got != tc.want {
			t.Errorf("headOpenEnd(%q) = %d, want %d", tc.doc, got, tc.want)
		}
	}
}
