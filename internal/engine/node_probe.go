package engine

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// NodeHealth holds the last-known health of all sibling services.
type NodeHealth struct {
	mu       sync.RWMutex
	services map[string]ServiceHealth
}

// ServiceHealth is the probed state of a single service.
type ServiceHealth struct {
	Port   int       `json:"port"`
	Status string    `json:"status"` // healthy, degraded, down
	At     time.Time `json:"probed_at"`
}

// NewNodeHealth returns an empty NodeHealth.
func NewNodeHealth() *NodeHealth {
	return &NodeHealth{services: make(map[string]ServiceHealth)}
}

// Probe checks all sibling services defined in the manifest.
// Skips the kernel itself (it knows its own health).
func (nh *NodeHealth) Probe(manifest *NodeManifest, selfPort int) {
	client := &http.Client{Timeout: 2 * time.Second}
	now := time.Now().UTC()

	for name, svc := range manifest.Services {
		if svc.Port == selfPort {
			continue // don't probe ourselves
		}

		status := "down"
		url := fmt.Sprintf("http://localhost:%d%s", svc.Port, svc.Health)
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				status = "healthy"
			} else {
				status = "degraded"
			}
		}

		nh.mu.Lock()
		nh.services[name] = ServiceHealth{
			Port:   svc.Port,
			Status: status,
			At:     now,
		}
		nh.mu.Unlock()
	}
}

// Snapshot returns a copy of the current service health map.
func (nh *NodeHealth) Snapshot() map[string]ServiceHealth {
	nh.mu.RLock()
	defer nh.mu.RUnlock()
	out := make(map[string]ServiceHealth, len(nh.services))
	for k, v := range nh.services {
		out[k] = v
	}
	return out
}

// Summary returns a compact status map (service → status string).
func (nh *NodeHealth) Summary() map[string]string {
	nh.mu.RLock()
	defer nh.mu.RUnlock()
	out := make(map[string]string, len(nh.services))
	for k, v := range nh.services {
		out[k] = v.Status
	}
	return out
}

// Counts returns (healthy, total) for quick reporting.
func (nh *NodeHealth) Counts() (int, int) {
	nh.mu.RLock()
	defer nh.mu.RUnlock()
	healthy := 0
	for _, v := range nh.services {
		if v.Status == "healthy" {
			healthy++
		}
	}
	return healthy, len(nh.services)
}

// Names returns sorted service names.
func (nh *NodeHealth) Names() []string {
	nh.mu.RLock()
	defer nh.mu.RUnlock()
	names := make([]string, 0, len(nh.services))
	for k := range nh.services {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
