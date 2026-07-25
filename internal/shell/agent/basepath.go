package agent

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Serving an app under a sub-path it does not know about.
//
// Most of the tunnel's apps are TOLD their base path at install (Grafana's serve_from_sub_path,
// Prometheus/Alertmanager's routePrefix), so every URL they emit already carries it and the agent
// only has to undo the API server's own rewrite. Longhorn cannot be told: longhorn-ui takes exactly
// one env var, LONGHORN_MANAGER_IP, and nothing that names a context route. (longhorn/longhorn#1660
// asked for one and is closed against v1.1.0, but no such setting exists in the image today -
// longhorn/longhorn#1745 and discussion #4727 are people still pushing /css and /js back under their
// subpath with nginx rewrite snippets, which is the same problem this solves.) It assumes it owns "/".
//
// Rewriting the HTML document gets the assets - the API server's service proxy rewrites the src/href
// attributes it finds, and the agent re-bases those onto the tunnel path. What it CANNOT reach are
// the URLs the SPA builds at runtime: `fetch("/v1/volumes")`, an XHR to "/v1/nodes", a WebSocket to
// "/v1/ws/1s/volumes", a router calling history.pushState("/dashboard"). Those strings live inside a
// minified bundle and are only assembled in the browser, where no proxy can see them.
//
// So the fix runs in the browser too: a small inline script, injected into the document head ahead of
// the app's own bundle, that wraps the four APIs through which every one of those URLs must pass and
// re-bases any root-relative path onto the tunnel prefix. It is a shim, and it is honest about being
// one: it works because a path is either root-relative (needs the prefix) or not (already correct),
// which is decidable without knowing anything about the app.
//
// PRODUCTION would not do this. It would give the app its own hostname - longhorn.<cluster>.<apps
// domain>, which the platform's wildcard DNS and default Gateway already make trivial - and let it
// serve from the root it expects. This exists because a demo control plane reaches clusters only
// through the API server's service proxy, where a per-app hostname is not available.

// basePathShim is the injected script, with %s replaced by the JSON-quoted route prefix.
//
// The guards matter more than the wrapping:
//   - only strings are touched (a fetch(Request) or a URL object passes through untouched);
//   - only paths starting with a single "/" - a relative path is already correct, and "//host/x" is
//     protocol-relative, i.e. another origin;
//   - never twice: a path already under the prefix is left alone, which is what keeps the wrappers
//     idempotent when the app round-trips a URL it was previously given.
const basePathShim = `<script>(function(){
var p=%s;
function fix(u){
if(typeof u!=="string"||u.charAt(0)!=="/"||u.charAt(1)==="/")return u;
if(u===p||u.indexOf(p+"/")===0)return u;
return p+u;
}
var f=window.fetch;
if(f)window.fetch=function(i,o){return f.call(this,typeof i==="string"?fix(i):i,o);};
var xo=XMLHttpRequest.prototype.open;
XMLHttpRequest.prototype.open=function(){arguments[1]=fix(arguments[1]);return xo.apply(this,arguments);};
var WS=window.WebSocket;
if(WS){
var W=function(u,pr){
if(typeof u==="string"&&u.indexOf("://")>-1){
try{var x=new URL(u);if(x.host===location.host)u=x.protocol+"//"+x.host+fix(x.pathname)+x.search;}catch(e){}
}else{u=fix(u);}
return arguments.length>1?new WS(u,pr):new WS(u);
};
W.prototype=WS.prototype;
W.CONNECTING=WS.CONNECTING;W.OPEN=WS.OPEN;W.CLOSING=WS.CLOSING;W.CLOSED=WS.CLOSED;
window.WebSocket=W;
}
var ps=history.pushState,rs=history.replaceState;
history.pushState=function(s,t,u){return ps.call(this,s,t,fix(u));};
history.replaceState=function(s,t,u){return rs.call(this,s,t,fix(u));};
})()</script>`

// injectBasePathShim places the shim at the top of the document's <head>, so it is evaluated before
// any of the app's own scripts and no request can be issued ahead of it. It falls back to prepending
// when there is no head (a fragment, or a document the app assembles oddly) - worst case the script
// still runs first, which is the only property that matters.
//
// Injecting once is enforced: this body may already carry the shim if the app is echoing back a
// document we served, and two layers of wrappers would double-prefix nothing but would double the
// work on every call.
func injectBasePathShim(body []byte, routePrefix string) []byte {
	if bytes.Contains(body, []byte("window.fetch=function")) {
		return body
	}
	// JSON-quoting the prefix is what keeps a cluster id from ever escaping the string literal. Ids
	// are generated, not user-supplied, but the prefix reaches here as an HTTP header - so it is
	// treated as untrusted input rather than trusted because of where it came from.
	quoted, err := json.Marshal(routePrefix)
	if err != nil {
		return body
	}
	shim := []byte(strings.Replace(basePathShim, "%s", string(quoted), 1))

	if i := headOpenEnd(body); i >= 0 {
		size := len(body) + len(shim)
		if size < len(body) { // overflow guard: len(shim) is fixed and small, so this only trips on a body near maxint
			return body
		}
		out := make([]byte, 0, size)
		out = append(out, body[:i]...)
		out = append(out, shim...)
		return append(out, body[i:]...)
	}
	return append(shim, body...)
}

// headOpenEnd returns the index just past the document's opening <head …> tag, or -1. Deliberately
// crude - a full HTML parse would buy nothing here, since the only documents that reach this path are
// an SPA's index.html and its error pages.
func headOpenEnd(body []byte) int {
	lower := bytes.ToLower(body)
	i := bytes.Index(lower, []byte("<head"))
	if i < 0 {
		return -1
	}
	end := bytes.IndexByte(lower[i:], '>')
	if end < 0 {
		return -1
	}
	return i + end + 1
}

// isHTML reports whether a Content-Type is an HTML document - narrower than rewritableBody, which
// also covers stylesheets. Only a document can carry the shim.
func isHTML(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/html")
}
