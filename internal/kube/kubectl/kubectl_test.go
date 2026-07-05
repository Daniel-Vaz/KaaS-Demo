package kubectl

import (
	"context"
	"strings"
	"testing"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
	"github.com/Daniel-Vaz/KaaS-demo/internal/kube"
)

// stubExecer returns canned kubectl JSON keyed by a substring of the joined args, so the tests
// exercise the real arg-building + parsing without a cluster.
type stubExecer struct{ responses map[string]string }

func (s stubExecer) Run(_ context.Context, _ []byte, _ string, args []string) (Result, error) {
	joined := strings.Join(args, " ")
	for key, body := range s.responses {
		if strings.Contains(joined, key) {
			return Result{Stdout: []byte(body)}, nil
		}
	}
	return Result{Stderr: "no stub for: " + joined, Code: 1}, nil
}

func (s stubExecer) Stream(context.Context, []byte, string, []string, kube.LogSink) error { return nil }

var cl = &domain.Cluster{ID: "c1", Name: "demo"}

const workloadsList = `{"items":[
  {"kind":"Deployment","metadata":{"name":"web","namespace":"demo","creationTimestamp":"2026-01-01T00:00:00Z"},
   "spec":{"replicas":3,"strategy":{"type":"RollingUpdate"},"selector":{"matchLabels":{"app":"web"}},
           "template":{"spec":{"containers":[{"name":"nginx","image":"nginx:1.27"}]}}},
   "status":{"readyReplicas":2,"updatedReplicas":3,"availableReplicas":2,
             "conditions":[{"type":"Available","status":"True","reason":"MinimumReplicasAvailable","lastUpdateTime":"2026-01-01T01:00:00Z"}]}},
  {"kind":"DaemonSet","metadata":{"name":"kube-proxy","namespace":"kube-system"},
   "spec":{"updateStrategy":{"type":"RollingUpdate"},"template":{"spec":{"containers":[{"name":"kube-proxy","image":"registry.k8s.io/kube-proxy:v1.36.2"}]}}},
   "status":{"desiredNumberScheduled":3,"numberReady":3}},
  {"kind":"Job","metadata":{"name":"db-migrate","namespace":"demo"},
   "spec":{"completions":1,"template":{"spec":{"containers":[{"name":"migrate","image":"migrate/migrate:v4.17.0"}]}}},
   "status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}},
  {"kind":"CronJob","metadata":{"name":"report","namespace":"demo"},
   "spec":{"schedule":"*/5 * * * *","suspend":false,"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"report","image":"busybox:1.36"}]}}}}},
   "status":{"active":[{"name":"report-28000000"}]}}
]}`

func TestWorkloadsParsing(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{"deployments,statefulsets": workloadsList}})
	ws, err := c.Workloads(context.Background(), cl, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 4 {
		t.Fatalf("got %d workloads, want 4", len(ws))
	}
	by := map[string]kube.WorkloadSummary{}
	for _, w := range ws {
		by[string(w.Kind)+"/"+w.Name] = w
	}

	web := by["deployment/web"]
	if web.DesiredReplicas != 3 || web.ReadyReplicas != 2 || web.Status != "Progressing" {
		t.Errorf("web = %d/%d %q, want 2/3 Progressing", web.ReadyReplicas, web.DesiredReplicas, web.Status)
	}
	if len(web.Images) != 1 || web.Images[0] != "nginx:1.27" {
		t.Errorf("web images = %v", web.Images)
	}

	ds := by["daemonset/kube-proxy"]
	if ds.DesiredReplicas != 3 || ds.ReadyReplicas != 3 || ds.Status != "Running" {
		t.Errorf("kube-proxy = %d/%d %q, want 3/3 Running", ds.ReadyReplicas, ds.DesiredReplicas, ds.Status)
	}

	job := by["job/db-migrate"]
	if job.Status != "Complete" || job.ReadyReplicas != 1 {
		t.Errorf("job = %q ready=%d, want Complete ready=1", job.Status, job.ReadyReplicas)
	}

	cj := by["cronjob/report"]
	if cj.Status != "Scheduled" || cj.Schedule != "*/5 * * * *" {
		t.Errorf("cronjob = %q sched=%q, want Scheduled */5", cj.Status, cj.Schedule)
	}
}

const deploymentObj = `{"kind":"Deployment","metadata":{"name":"web","namespace":"demo","labels":{"app":"web"}},
  "spec":{"replicas":2,"strategy":{"type":"RollingUpdate"},"selector":{"matchLabels":{"app":"web"}},
          "template":{"spec":{"containers":[{"name":"nginx","image":"nginx:1.27"}]}}},
  "status":{"readyReplicas":2,"updatedReplicas":2,"availableReplicas":2}}`

const podsList = `{"items":[
  {"metadata":{"name":"web-abc-1","namespace":"demo"},
   "spec":{"nodeName":"demo-w-0","containers":[{"name":"nginx"}]},
   "status":{"phase":"Running","podIP":"10.244.1.5","containerStatuses":[{"name":"nginx","ready":true,"restartCount":1}]}},
  {"metadata":{"name":"web-abc-2","namespace":"demo"},
   "spec":{"nodeName":"demo-w-1","containers":[{"name":"nginx"}]},
   "status":{"phase":"Pending","containerStatuses":[{"name":"nginx","ready":false,"restartCount":0,"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}
]}`

func TestWorkloadDetailWithPods(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{
		"get deployments web": deploymentObj,
		"get pods":            podsList,
	}})
	d, err := c.Workload(context.Background(), cl, nil, kube.WorkloadRef{Kind: kube.KindDeployment, Namespace: "demo", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Strategy != "RollingUpdate" || d.Selector["app"] != "web" {
		t.Errorf("strategy/selector = %q %v", d.Strategy, d.Selector)
	}
	if len(d.Pods) != 2 {
		t.Fatalf("got %d pods, want 2", len(d.Pods))
	}
	p0 := d.Pods[0]
	if p0.Ready != "1/1" || p0.Restarts != 1 || p0.Node != "demo-w-0" || p0.Status != "Running" {
		t.Errorf("pod0 = %+v", p0)
	}
	// A waiting container reason should surface over the raw phase.
	if d.Pods[1].Status != "ImagePullBackOff" {
		t.Errorf("pod1 status = %q, want ImagePullBackOff", d.Pods[1].Status)
	}
}

const eventsList = `{"items":[
  {"type":"Normal","reason":"ScalingReplicaSet","message":"Scaled up","count":1,"lastTimestamp":"2026-01-01T00:00:00Z","involvedObject":{"kind":"Deployment","name":"web"}},
  {"type":"Warning","reason":"BackOff","message":"Back-off restarting","count":3,"lastTimestamp":"2026-01-01T02:00:00Z","involvedObject":{"kind":"Pod","name":"web-abc-2"}},
  {"type":"Normal","reason":"Pulled","message":"other","count":1,"lastTimestamp":"2026-01-01T03:00:00Z","involvedObject":{"kind":"Pod","name":"other-xyz"}}
]}`

func TestEventsFilterAndSort(t *testing.T) {
	c := New(stubExecer{responses: map[string]string{"get events": eventsList}})
	ev, err := c.Events(context.Background(), cl, nil, kube.WorkloadRef{Kind: kube.KindDeployment, Namespace: "demo", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 2 {
		t.Fatalf("got %d events, want 2 (web + web-abc-2, not other-xyz)", len(ev))
	}
	// Newest first.
	if ev[0].Reason != "BackOff" || ev[0].Count != 3 {
		t.Errorf("first event = %+v, want BackOff count 3", ev[0])
	}
}
