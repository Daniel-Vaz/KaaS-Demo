package kube

// Wire types for the API↔worker exec channel (internal/kube/proxy → internal/shell/agent). The API
// can't reach cluster API servers, so in real mode it forwards each kubectl invocation to the
// host-networked worker's exec agent. Kept here (a neutral package both sides import) rather than in
// proxy, so the agent needn't import an API-side package.

// ExecRequest is the /kube-exec POST body: run `kubectl <args>` against this cluster's kubeconfig.
// Stdin, when set, is fed to the command's standard input - needed for `kubectl create -f -` (the
// per-user kubeconfig mint submits a CertificateSigningRequest this way); nil for every read/scale
// call, which take no input.
type ExecRequest struct {
	ClusterID  string   `json:"cluster_id"`
	Kubeconfig []byte   `json:"kubeconfig"`
	Args       []string `json:"args"`
	Stdin      []byte   `json:"stdin,omitempty"`
}

// ExecResponse is the /kube-exec result. Error is set only for a failure to run kubectl at all
// (couldn't launch); a non-zero kubectl exit rides in Code+Stderr, mirroring LocalExecer.Run.
type ExecResponse struct {
	Stdout []byte `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   int    `json:"code"`
	Error  string `json:"error,omitempty"`
}

// LogsRequest is the first (text) frame on the /kube-logs WebSocket: the log tail/follow command to
// stream. Subsequent frames from the worker are raw log bytes (binary) or a JSON error (text).
type LogsRequest struct {
	ClusterID  string   `json:"cluster_id"`
	Kubeconfig []byte   `json:"kubeconfig"`
	Args       []string `json:"args"`
}
