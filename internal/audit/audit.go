// Package audit is the request-driven cluster API-audit seam behind the portal's Audit tab: it
// reads the Kubernetes API server's audit events from a Ready cluster and returns a filtered,
// summarized page the portal renders as a "who changed what" activity feed.
//
// Audit logging is enabled by default on every cluster (the ansible controlplane_audit role patches
// the kube-apiserver static pod with an audit policy + a stdout backend - see
// docs/configuration-management.md). The API server writes each event as one JSON line to its OWN
// stdout, so reading them back is just `kubectl logs kube-apiserver-<node>` - the SAME worker exec
// agent the Workloads/Monitoring/Security seams use (internal/audit/kubectl). No new in-cluster
// component and no new API<->worker transport is added. The Fake synthesizes a plausible, live-drifting
// stream from cluster state so the whole tab is demoable under make up-fake.
//
// Fidelity/security shortcut (shared with monitoring/security): the read runs with the cluster ADMIN
// kubeconfig server-side, because reading the apiserver pod's log is not something the built-in `view`
// role grants; the data returned is read-only audit metadata and access is still gated by the app's
// owner/group/admin view check. Two deliberate shortcuts, noted in the repo's style: the apiserver
// logs audit to its own stdout rather than a real sink (production ships to Loki/ELK/a webhook), and
// the read uses the admin kubeconfig rather than a scoped token.
package audit

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// Tuning constants for the read. TailLines is how many trailing apiserver log lines the real querier
// fetches per control plane before parsing; DefaultLimit/MaxLimit bound how many parsed events a page
// returns to the portal.
const (
	TailLines    = 2000
	DefaultLimit = 200
	MaxLimit     = 1000
)

// Resource is the Kubernetes object an audit event acted on (empty fields for a non-resource request
// like a discovery or health probe).
type Resource struct {
	APIGroup    string `json:"api_group,omitempty"`   // "" for core/v1, "apps", "rbac.authorization.k8s.io", …
	Type        string `json:"resource,omitempty"`    // "pods", "deployments", "secrets", …
	Subresource string `json:"subresource,omitempty"` // "status", "scale", "log", …
	Namespace   string `json:"namespace,omitempty"`   // empty for cluster-scoped objects
	Name        string `json:"name,omitempty"`
}

// Event is one API-server audit record, flattened to what the Audit tab shows.
type Event struct {
	AuditID      string   `json:"audit_id"`
	Timestamp    string   `json:"timestamp"` // stageTimestamp, RFC3339
	Stage        string   `json:"stage,omitempty"`
	Level        string   `json:"level,omitempty"`
	Verb         string   `json:"verb"`
	User         string   `json:"user"`
	Groups       []string `json:"groups,omitempty"`
	SourceIPs    []string `json:"source_ips,omitempty"`
	UserAgent    string   `json:"user_agent,omitempty"`
	Resource     Resource `json:"resource"`
	RequestURI   string   `json:"request_uri,omitempty"`
	ResponseCode int      `json:"response_code,omitempty"`
}

// Denied reports whether the request failed (HTTP >= 400): forbidden, conflict, not-found, etc.
func (e Event) Denied() bool { return e.ResponseCode >= 400 }

// Query is the filter/paging the portal sends. Zero value is "newest DefaultLimit events, no filter".
type Query struct {
	Limit     int    // 0 → DefaultLimit; capped at MaxLimit
	Verb      string // exact verb match (case-insensitive)
	Namespace string // exact namespace match (case-insensitive)
	User      string // substring of the username
	Resource  string // substring of the resource type
	Search    string // substring across user/verb/resource/name/namespace/requestURI
}

func (q Query) normalized() Query {
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	return q
}

func (q Query) matches(e Event) bool {
	if q.Verb != "" && !strings.EqualFold(e.Verb, q.Verb) {
		return false
	}
	if q.Namespace != "" && !strings.EqualFold(e.Resource.Namespace, q.Namespace) {
		return false
	}
	if q.User != "" && !containsFold(e.User, q.User) {
		return false
	}
	if q.Resource != "" && !containsFold(e.Resource.Type, q.Resource) {
		return false
	}
	if s := q.Search; s != "" {
		if !containsFold(e.User, s) && !containsFold(e.Verb, s) && !containsFold(e.Resource.Type, s) &&
			!containsFold(e.Resource.Name, s) && !containsFold(e.Resource.Namespace, s) && !containsFold(e.RequestURI, s) {
			return false
		}
	}
	return true
}

// VerbCount is one verb's occurrence count in a page, for the stat tiles.
type VerbCount struct {
	Verb  string `json:"verb"`
	Count int    `json:"count"`
}

// Stats is the page's rollup: the counts the Audit tab shows as tiles/chips.
type Stats struct {
	Total      int         `json:"total"`      // events in this page
	Denied     int         `json:"denied"`     // events with response code >= 400
	Users      int         `json:"users"`      // distinct usernames in this page
	Namespaces int         `json:"namespaces"` // distinct namespaces touched in this page
	ByVerb     []VerbCount `json:"by_verb"`    // verb breakdown, most-frequent first
}

// Page is one Audit-tab response: the newest matching events (capped), plus their rollup.
type Page struct {
	Events      []Event `json:"events"`
	Stats       Stats   `json:"stats"`
	GeneratedAt string  `json:"generated_at"`
	Truncated   bool    `json:"truncated"` // more matched than Limit - the feed is capped
}

// Enabled reports whether the cluster can serve audit events. Audit logging is baked into every
// control plane at bootstrap, so the only gate is that the cluster is Ready (its apiserver reachable).
// A cluster provisioned before audit was enabled simply returns an empty page - not an error.
func Enabled(c *domain.Cluster) bool { return c.Phase == domain.PhaseReady }

// Querier reads audit events from a Ready cluster given its admin kubeconfig. Every call is
// per-request; a transient failure is a normal error the API surfaces, never fatal.
type Querier interface {
	Events(ctx context.Context, c *domain.Cluster, kubeconfig []byte, q Query) (*Page, error)
}

// Assemble is the shared tail of both the Fake and the real querier: given the raw events collected
// from a cluster (possibly across several apiserver pods, in any order, with duplicates), it dedupes
// by audit ID, sorts newest-first, applies the query's filters, caps to the limit, and computes the
// rollup - so both implementations return an identically-shaped, identically-ordered page.
func Assemble(events []Event, q Query) *Page {
	q = q.normalized()
	// Newest first; audit ID as a stable tiebreaker for equal timestamps.
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Timestamp != events[j].Timestamp {
			return events[i].Timestamp > events[j].Timestamp
		}
		return events[i].AuditID > events[j].AuditID
	})
	seen := make(map[string]bool, len(events))
	matched := make([]Event, 0, len(events))
	for _, e := range events {
		if e.AuditID != "" {
			if seen[e.AuditID] {
				continue // same request seen on two apiservers / two stages - keep the newest
			}
			seen[e.AuditID] = true
		}
		if q.matches(e) {
			matched = append(matched, e)
		}
	}
	truncated := len(matched) > q.Limit
	if truncated {
		matched = matched[:q.Limit]
	}
	return &Page{
		Events:      matched,
		Stats:       statsOf(matched),
		GeneratedAt: nowRFC3339(),
		Truncated:   truncated,
	}
}

func statsOf(events []Event) Stats {
	st := Stats{Total: len(events)}
	users := map[string]bool{}
	namespaces := map[string]bool{}
	verbs := map[string]int{}
	for _, e := range events {
		if e.User != "" {
			users[e.User] = true
		}
		if e.Resource.Namespace != "" {
			namespaces[e.Resource.Namespace] = true
		}
		if e.Denied() {
			st.Denied++
		}
		if e.Verb != "" {
			verbs[e.Verb]++
		}
	}
	st.Users = len(users)
	st.Namespaces = len(namespaces)
	for v, n := range verbs {
		st.ByVerb = append(st.ByVerb, VerbCount{Verb: v, Count: n})
	}
	sort.Slice(st.ByVerb, func(i, j int) bool {
		if st.ByVerb[i].Count != st.ByVerb[j].Count {
			return st.ByVerb[i].Count > st.ByVerb[j].Count
		}
		return st.ByVerb[i].Verb < st.ByVerb[j].Verb
	})
	return st
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
