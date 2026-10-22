//go:build linux

package main

import (
	"fmt"
	"net/http"

	"github.com/mightbeanshuu/limitplane/xdp/fastpath"
)

// metricsHandler serves the kernel counters in Prometheus text format, by
// hand, the same way the gateway's /metrics does — no client library.
func metricsHandler(m *fastpath.Maps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := m.Read()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		keys, err := m.BannedKeys()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# HELP limitplane_xdp_packets_total Packets seen by the XDP fast path, by verdict.")
		fmt.Fprintln(w, "# TYPE limitplane_xdp_packets_total counter")
		fmt.Fprintf(w, "limitplane_xdp_packets_total{verdict=\"passed\"} %d\n", c.Passed)
		fmt.Fprintf(w, "limitplane_xdp_packets_total{verdict=\"dropped_banned\"} %d\n", c.DroppedBanned)
		fmt.Fprintf(w, "limitplane_xdp_packets_total{verdict=\"dropped_rate\"} %d\n", c.DroppedRate)
		fmt.Fprintln(w, "# HELP limitplane_xdp_banned_ips Addresses currently on the kernel ban list.")
		fmt.Fprintln(w, "# TYPE limitplane_xdp_banned_ips gauge")
		fmt.Fprintf(w, "limitplane_xdp_banned_ips %d\n", len(keys))
	})
}
