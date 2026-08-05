//go:build js

package execagent

import "github.com/coder/websocket"

// DialOptions is the js/wasm counterpart of the real one: a browser WebSocket handshake carries no
// custom headers, so the bearer token cannot be sent. Nothing in the js build dials an exec agent
// (cmd/demo-wasm runs every seam as a fake), so this only has to exist, not work.
func DialOptions(string) *websocket.DialOptions { return &websocket.DialOptions{} }
