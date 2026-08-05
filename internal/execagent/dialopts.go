//go:build !js

package execagent

import (
	"net/http"

	"github.com/coder/websocket"
)

// DialOptions builds the WebSocket dial options for a call to an exec agent, authenticating with
// the shared bearer token (empty token = no header, for a deployment that runs without one).
//
// It exists as a build-tagged seam because the js/wasm build cannot set it: a browser's WebSocket
// handshake takes no custom headers. That build (cmd/demo-wasm) never selects a proxy backend -
// there is no exec agent to reach from a browser - so the js variant is a no-op rather than an
// alternative credential path.
func DialOptions(token string) *websocket.DialOptions {
	opts := &websocket.DialOptions{}
	if token != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + token}}
	}
	return opts
}
