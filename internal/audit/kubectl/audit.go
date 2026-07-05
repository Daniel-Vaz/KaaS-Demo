// Package kubectl is the real audit.Querier: it reads the Kubernetes API server's audit events from a
// cluster by tailing the kube-apiserver static pod's log - where the audit backend writes them as JSON
// lines (--audit-log-path=-, configured by the ansible controlplane_audit role). It runs through the
// same Execer the Workloads/Monitoring/Security seams use (a LocalExecer on the worker, or the API-side
// proxy that forwards to the worker exec agent), so no new API<->worker transport is added: an audit
// read is just `kubectl get pods` + `kubectl logs`.
//
// For an HA cluster there is one apiserver pod per control plane, each auditing the requests IT served,
// so the querier tails every apiserver pod and merges the results (audit.Assemble dedupes + orders) -
// tailed in PARALLEL, since an HA read is otherwise three serial multi-megabyte log pulls.
//
// The fetched window is cached per cluster for a few seconds (see cache). A tail is expensive - the
// apiserver's stdout is interleaved klog + audit JSON, so TailLines lines is megabytes over the
// API->agent hop (and, on a remote KVM host, the SOCKS tunnel) - while the Audit tab re-reads it on
// every filter change, every keystroke and every 5s poll, in every open tab. Filtering happens in
// audit.Assemble, ABOVE the fetch, so a cached window serves any query: only the poll pays for kubectl.
package kubectl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Daniel-Vaz/KaaS-demo/internal/audit"
	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube/kubectl"
)

// Execer runs a one-shot kubectl command for a cluster (structurally satisfied by the Workloads seam's
// LocalExecer and proxy Execer). Only Run is needed - the audit seam tails a bounded number of lines
// rather than streaming.
type Execer interface {
	Run(ctx context.Context, kubeconfig []byte, clusterID string, args []string) (kubectl.Result, error)
}

// Querier implements audit.Querier on top of an Execer.
type Querier struct {
	ex    Execer
	cache *cache
}

// New returns a kubectl-backed audit.Querier using ex to run kubectl.
func New(ex Execer) *Querier { return &Querier{ex: ex, cache: newCache(CacheTTL)} }

func (q *Querier) Events(ctx context.Context, c *domain.Cluster, kc []byte, query audit.Query) (*audit.Page, error) {
	events, err := q.cache.window(ctx, c.ID, func(ctx context.Context) ([]audit.Event, error) {
		return q.fetch(ctx, c, kc)
	})
	if err != nil {
		return nil, err
	}
	return audit.Assemble(events, query), nil
}

// fetch pulls the current audit window from every apiserver pod, in parallel.
func (q *Querier) fetch(ctx context.Context, c *domain.Cluster, kc []byte) ([]audit.Event, error) {
	pods, err := q.apiserverPods(ctx, c, kc)
	if err != nil {
		return nil, err
	}
	var (
		mu     sync.Mutex
		events []audit.Event
		ok     bool // at least one apiserver answered
		wg     sync.WaitGroup
	)
	for _, pod := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evs, err := q.tailPod(ctx, c, kc, pod)
			if err != nil {
				// A control plane can be momentarily unreachable (rolling replacement, a just-joined
				// CP); skip that apiserver rather than failing the whole read - the others still
				// return events.
				return
			}
			mu.Lock()
			defer mu.Unlock()
			events = append(events, evs...)
			ok = true
		}()
	}
	wg.Wait()
	if len(pods) > 0 && !ok {
		// Every apiserver failed: a real read failure, not an empty trail. Returning an error keeps
		// it OUT of the cache, so the next poll retries immediately instead of serving an empty feed.
		return nil, fmt.Errorf("no apiserver returned audit events")
	}
	return events, nil
}

// apiserverPods lists the kube-apiserver static-pod names (one per control plane).
func (q *Querier) apiserverPods(ctx context.Context, c *domain.Cluster, kc []byte) ([]string, error) {
	args := []string{"get", "pods", "-n", "kube-system", "-l", "component=kube-apiserver",
		"-o", "jsonpath={.items[*].metadata.name}"}
	res, err := q.ex.Run(ctx, kc, c.ID, args)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("kubectl get apiserver pods: %s", firstLine(res.Stderr))
	}
	return strings.Fields(string(res.Stdout)), nil
}

// tailPod fetches the trailing audit lines from one apiserver pod and parses the audit events out of
// them. The apiserver's own klog output is interleaved on the same stream, so non-JSON / non-audit
// lines are simply skipped.
func (q *Querier) tailPod(ctx context.Context, c *domain.Cluster, kc []byte, pod string) ([]audit.Event, error) {
	args := []string{"logs", "-n", "kube-system", pod, fmt.Sprintf("--tail=%d", audit.TailLines)}
	res, err := q.ex.Run(ctx, kc, c.ID, args)
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("kubectl logs %s: %s", pod, firstLine(res.Stderr))
	}
	return parseAuditLog(res.Stdout), nil
}

// auditAPIVersion is the marker every audit line carries. Matching it as raw bytes lets us skip the
// json.Unmarshal for lines that can't be audit events - the tail is mostly klog noise, and unmarshalling
// megabytes of it into rawEvent only to discard it is the parse's whole cost.
var auditAPIVersion = []byte(`audit.k8s.io/`)

// parseAuditLog extracts audit events from an apiserver log tail: one JSON audit Event per line,
// interleaved with klog lines that don't parse as an audit Event (and are skipped). It scans the
// bytes in place - a tail is multiple megabytes, so a string conversion + Split would copy it twice.
func parseAuditLog(logs []byte) []audit.Event {
	var out []audit.Event
	for len(logs) > 0 {
		line := logs
		if i := bytes.IndexByte(logs, '\n'); i >= 0 {
			line, logs = logs[:i], logs[i+1:]
		} else {
			logs = nil
		}
		line = bytes.TrimSpace(line)
		// klog lines start with a level letter (I/W/E/F); audit events are a JSON object.
		if len(line) == 0 || line[0] != '{' || !bytes.Contains(line, auditAPIVersion) {
			continue
		}
		var raw rawEvent
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		if raw.Kind != "Event" || !strings.HasPrefix(raw.APIVersion, "audit.k8s.io/") {
			continue
		}
		out = append(out, raw.event())
	}
	return out
}

// rawEvent is the subset of a kube-apiserver audit.k8s.io Event we surface.
type rawEvent struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	AuditID    string `json:"auditID"`
	Stage      string `json:"stage"`
	Level      string `json:"level"`
	Verb       string `json:"verb"`
	RequestURI string `json:"requestURI"`
	User       struct {
		Username string   `json:"username"`
		Groups   []string `json:"groups"`
	} `json:"user"`
	SourceIPs []string `json:"sourceIPs"`
	UserAgent string   `json:"userAgent"`
	ObjectRef struct {
		Resource    string `json:"resource"`
		Namespace   string `json:"namespace"`
		Name        string `json:"name"`
		APIGroup    string `json:"apiGroup"`
		Subresource string `json:"subresource"`
	} `json:"objectRef"`
	ResponseStatus struct {
		Code int `json:"code"`
	} `json:"responseStatus"`
	StageTimestamp           string `json:"stageTimestamp"`
	RequestReceivedTimestamp string `json:"requestReceivedTimestamp"`
}

func (r rawEvent) event() audit.Event {
	ts := r.StageTimestamp
	if ts == "" {
		ts = r.RequestReceivedTimestamp
	}
	return audit.Event{
		AuditID:      r.AuditID,
		Timestamp:    ts,
		Stage:        r.Stage,
		Level:        r.Level,
		Verb:         r.Verb,
		User:         r.User.Username,
		Groups:       r.User.Groups,
		SourceIPs:    r.SourceIPs,
		UserAgent:    r.UserAgent,
		RequestURI:   r.RequestURI,
		ResponseCode: r.ResponseStatus.Code,
		Resource: audit.Resource{
			APIGroup:    r.ObjectRef.APIGroup,
			Type:        r.ObjectRef.Resource,
			Subresource: r.ObjectRef.Subresource,
			Namespace:   r.ObjectRef.Namespace,
			Name:        r.ObjectRef.Name,
		},
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "no output"
	}
	return s
}
