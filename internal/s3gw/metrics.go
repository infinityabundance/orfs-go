package s3gw

import (
	"io"
	"net/http"

	s3metrics "github.com/infinityabundance/orfs-go/internal/metrics"
	"github.com/infinityabundance/orfs-go/internal/observe"
)

// handleMetrics renders the Prometheus text exposition for the default volume,
// plus the gateway's own request counters.
//
// The OrFS families (orfs_physical_bytes, orfs_authoritative_bytes,
// orfs_logical_bytes, orfs_manifested_bytes, orfs_amplification_ratio,
// orfs_breach_total, orfs_quarantine_total, orfs_verify_deep_failures, the
// barrier summary and the group-commit gauges) come from internal/observe over
// internal/metrics, exactly as `orfs metrics` renders them, so existing
// dashboards and rate(breach)/rate(quarantine) queries keep working. The
// runtime counters (breach, barrier, batch) are the live process's, so they are
// read from the volume rather than from a persisted snapshot.
func (g *Gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	vs := g.cfg.Default
	if vs == nil {
		http.Error(w, "no default OrFS volume is configured for metrics", http.StatusNotFound)
		return
	}
	rt := vs.Vol.Log.Runtime()
	snap, err := observe.Collect(vs.Vol, observe.Options{Runtime: &rt})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	set := snap.Set()
	set.Counter("orfs_s3_requests_total", "S3 gateway requests by operation.",
		float64(g.puts.Load()), s3metrics.Label{Name: "op", Value: "PutObject"})
	set.Counter("orfs_s3_requests_total", "S3 gateway requests by operation.",
		float64(g.gets.Load()), s3metrics.Label{Name: "op", Value: "GetObject"})
	set.Counter("orfs_s3_requests_total", "S3 gateway requests by operation.",
		float64(g.heads.Load()), s3metrics.Label{Name: "op", Value: "HeadObject"})
	set.Counter("orfs_s3_requests_total", "S3 gateway requests by operation.",
		float64(g.deletes.Load()), s3metrics.Label{Name: "op", Value: "DeleteObject"})
	set.Counter("orfs_s3_requests_total", "S3 gateway requests by operation.",
		float64(g.lists.Load()), s3metrics.Label{Name: "op", Value: "ListObjects"})
	set.Counter("orfs_s3_worm_refusals_total",
		"WORM refusals (a write or withdraw against a locked object) by reason.",
		float64(g.refusedRetention.Load()), s3metrics.Label{Name: "reason", Value: "retention"})
	set.Counter("orfs_s3_worm_refusals_total",
		"WORM refusals (a write or withdraw against a locked object) by reason.",
		float64(g.refusedLegalHold.Load()), s3metrics.Label{Name: "reason", Value: "legal-hold"})

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	io.WriteString(w, set.Text())
}
