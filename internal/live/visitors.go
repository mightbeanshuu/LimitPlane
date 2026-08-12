package live

import (
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/mightbeanshuu/limitplane/internal/useragent"
)

// Visitor is one unique client IP, geolocated for the live map and labelled
// with the device it came from.
type Visitor struct {
	IP       string  `json:"ip"`
	Count    int64   `json:"count"`
	LastSeen int64   `json:"lastSeen"`
	Geo      string  `json:"geo"` // "local" | "pending" | "ok" | "unknown"
	City     string  `json:"city,omitempty"`
	Country  string  `json:"country,omitempty"`
	Lat      float64 `json:"lat,omitempty"`
	Lon      float64 `json:"lon,omitempty"`
	Device   string  `json:"device,omitempty"`
	OS       string  `json:"os,omitempty"`
	Browser  string  `json:"browser,omitempty"`
	Label    *string `json:"label,omitempty"` // behavioural class, stamped by the caller
}

// Visitors tracks unique client IPs for the dashboard map.
//
// Geolocation is looked up once per IP from a free public API, off the request
// path in a goroutine — a slow third party must never delay the request that
// triggered the lookup. Private and loopback addresses are labelled instead of
// looked up, because sending them to a geo service is both pointless and a
// small privacy leak.
type Visitors struct {
	now    func() int64
	client *http.Client
	maxIPs int

	mu    sync.Mutex
	byIP  map[string]*Visitor
	order []string
}

func NewVisitors(now func() int64, client *http.Client, maxIPs int) *Visitors {
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if maxIPs <= 0 {
		maxIPs = 500
	}
	return &Visitors{now: now, client: client, maxIPs: maxIPs, byIP: map[string]*Visitor{}}
}

// IsPrivate reports whether an address is loopback, link-local or in a private
// range — anything that a public geo lookup could not resolve anyway.
func IsPrivate(ip string) bool {
	if ip == "" || ip == "unknown" {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return true
	}
	return parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast() ||
		parsed.IsLinkLocalMulticast() || parsed.IsUnspecified()
}

// Track records one hit from an IP, kicking off a geo lookup the first time.
func (v *Visitors) Track(ip, ua string) {
	if ip == "" {
		return
	}

	v.mu.Lock()
	visitor, existed := v.byIP[ip]
	if !existed {
		geo := "pending"
		if IsPrivate(ip) {
			geo = "local"
		}
		visitor = &Visitor{IP: ip, Geo: geo}
		v.byIP[ip] = visitor
		v.order = append(v.order, ip)
		if len(v.order) > v.maxIPs {
			oldest := v.order[0]
			v.order = v.order[1:]
			delete(v.byIP, oldest)
		}
	}
	if ua != "" && visitor.OS == "" {
		p := useragent.Parse(ua)
		visitor.Device, visitor.OS, visitor.Browser = p.Device, p.OS, p.Browser
	}
	visitor.Count++
	visitor.LastSeen = v.now()
	needsGeo := !existed && visitor.Geo == "pending"
	v.mu.Unlock()

	if needsGeo {
		go v.lookup(ip)
	}
}

func (v *Visitors) lookup(ip string) {
	defer func() { _ = recover() }()

	resp, err := v.client.Get("http://ip-api.com/json/" + ip + "?fields=status,city,country,lat,lon")
	status := "unknown"
	var body struct {
		Status  string  `json:"status"`
		City    string  `json:"city"`
		Country string  `json:"country"`
		Lat     float64 `json:"lat"`
		Lon     float64 `json:"lon"`
	}
	if err == nil {
		defer resp.Body.Close()
		if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Status == "success" {
			status = "ok"
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	visitor, ok := v.byIP[ip]
	if !ok {
		return // evicted while the lookup was in flight
	}
	visitor.Geo = status
	if status == "ok" {
		visitor.City, visitor.Country, visitor.Lat, visitor.Lon = body.City, body.Country, body.Lat, body.Lon
	}
}

// Snapshot returns copies of every tracked visitor. `label` stamps each one
// with its behavioural class; pass nil to skip.
func (v *Visitors) Snapshot(label func(ip string) *string) []Visitor {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]Visitor, 0, len(v.byIP))
	for _, visitor := range v.byIP {
		copied := *visitor
		if label != nil {
			copied.Label = label(copied.IP)
		}
		out = append(out, copied)
	}
	return out
}

func (v *Visitors) Count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.byIP)
}
