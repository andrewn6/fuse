package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// PrometheusMetrics implements orchestrator.ReconcileMetrics using Prometheus
// counters, gauges, and histograms. It also exposes VM lifecycle
// gauges that are updated each reconcile cycle.
type PrometheusMetrics struct {
	// Reconcile cycle metrics.
	reconcileDuration   prometheus.Histogram
	reconcileTotal      prometheus.Counter
	trackedVMs          prometheus.Gauge
	providerVMs         prometheus.Gauge
	orphansDestroyed    prometheus.Counter
	orphansFailed       prometheus.Counter
	orphansDeadLettered prometheus.Counter
	stuckTasksSuspected prometheus.Counter
	stuckTasksFailed    prometheus.Counter
	idleVMsSuspected    prometheus.Counter
	idleVMsFailed       prometheus.Counter
	vmsMissingProvider  prometheus.Counter

	// HTTP handler metrics (used by the middleware).
	HTTPRequestsTotal    *prometheus.CounterVec
	HTTPRequestDuration  *prometheus.HistogramVec
	HTTPRequestsInFlight prometheus.Gauge
}

// NewPrometheusMetrics creates and registers all orchestrator metrics
// on the given registry. Pass prometheus.DefaultRegisterer for the
// global registry, or a custom one for tests.
func NewPrometheusMetrics(reg prometheus.Registerer) *PrometheusMetrics {
	m := &PrometheusMetrics{
		reconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "duration_seconds",
			Help:      "Time spent in a single reconcile cycle.",
			Buckets:   prometheus.DefBuckets,
		}),
		reconcileTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "cycles_total",
			Help:      "Total number of reconcile cycles completed.",
		}),
		trackedVMs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "orchestrator",
			Subsystem: "fleet",
			Name:      "tracked_vms",
			Help:      "Number of VMs currently tracked by the fleet manager.",
		}),
		providerVMs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "orchestrator",
			Subsystem: "fleet",
			Name:      "provider_vms",
			Help:      "Number of VMs reported by the provider(s).",
		}),
		orphansDestroyed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "orphans_destroyed_total",
			Help:      "Total orphan VMs successfully destroyed.",
		}),
		orphansFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "orphans_failed_total",
			Help:      "Total orphan VM destroy failures.",
		}),
		orphansDeadLettered: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "orphans_dead_lettered_total",
			Help:      "Total orphan VMs dead-lettered after max retries.",
		}),
		stuckTasksSuspected: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "stuck_tasks_suspected_total",
			Help:      "Total tasks suspected stuck (first strike).",
		}),
		stuckTasksFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "stuck_tasks_failed_total",
			Help:      "Total tasks failed due to being stuck (second strike).",
		}),
		idleVMsSuspected: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "idle_vms_suspected_total",
			Help:      "Total VMs suspected idle (first strike).",
		}),
		idleVMsFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "idle_vms_failed_total",
			Help:      "Total VMs torn down for exceeding their idle timeout (second strike).",
		}),
		vmsMissingProvider: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "reconcile",
			Name:      "vms_missing_provider_total",
			Help:      "Total VMs that vanished from the provider.",
		}),

		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "orchestrator",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "Total HTTP requests by route, method, and status code.",
		}, []string{"route", "method", "code"}),
		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "orchestrator",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency by route and method.",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route", "method"}),
		HTTPRequestsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "orchestrator",
			Subsystem: "http",
			Name:      "requests_in_flight",
			Help:      "Number of HTTP requests currently being served.",
		}),
	}

	reg.MustRegister(
		m.reconcileDuration,
		m.reconcileTotal,
		m.trackedVMs,
		m.providerVMs,
		m.orphansDestroyed,
		m.orphansFailed,
		m.orphansDeadLettered,
		m.stuckTasksSuspected,
		m.stuckTasksFailed,
		m.idleVMsSuspected,
		m.idleVMsFailed,
		m.vmsMissingProvider,
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.HTTPRequestsInFlight,
	)

	return m
}

// ReconcileCompleted implements the orchestrator.ReconcileMetrics interface. Called
// at the end of each reconcile cycle with a summary of what happened.
func (m *PrometheusMetrics) ReconcileCompleted(s orchestrator.ReconcileSummary) {
	m.reconcileTotal.Inc()
	m.reconcileDuration.Observe(s.Duration.Seconds())
	m.trackedVMs.Set(float64(s.TrackedVMs))
	m.providerVMs.Set(float64(s.ProviderVMs))
	m.orphansDestroyed.Add(float64(s.OrphansDestroyed))
	m.orphansFailed.Add(float64(s.OrphansFailed))
	m.orphansDeadLettered.Add(float64(s.OrphansDeadLettered))
	m.stuckTasksSuspected.Add(float64(s.StuckTasksSuspected))
	m.stuckTasksFailed.Add(float64(s.StuckTasksFailed))
	m.idleVMsSuspected.Add(float64(s.IdleVMsSuspected))
	m.idleVMsFailed.Add(float64(s.IdleVMsFailed))
	m.vmsMissingProvider.Add(float64(s.VMsMissingProvider))
}
