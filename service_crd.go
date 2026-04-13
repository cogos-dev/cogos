// service_crd.go — Service CRD types and YAML loader.
//
// Follows the agent_crd.go pattern. Service definitions live at
// .cog/config/services/{name}.service.yaml and declare containerized
// services managed by the CogOS container lifecycle.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ─── CRD Types ──────────────────────────────────────────────────────────────────

// ServiceCRD is the top-level Kubernetes-style service definition.
type ServiceCRD struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   ServiceCRDMeta `yaml:"metadata"`
	Spec       ServiceCRDSpec `yaml:"spec"`
}

// ServiceCRDMeta holds service metadata.
type ServiceCRDMeta struct {
	Name        string            `yaml:"name"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// ServiceCRDSpec defines the service specification.
type ServiceCRDSpec struct {
	Image     string           `yaml:"image"`
	Platform  string           `yaml:"platform,omitempty"`
	Ports     []ServicePort    `yaml:"ports,omitempty"`
	Env       []string         `yaml:"env,omitempty"`
	Resources ServiceResources `yaml:"resources,omitempty"`
	Volumes   []ServiceVolume  `yaml:"volumes,omitempty"`
	Health    ServiceHealth    `yaml:"health,omitempty"`
	Restart   string           `yaml:"restart,omitempty"`
	Tools      []ServiceTool    `yaml:"tools,omitempty"`
	Modalities []string         `yaml:"modalities,omitempty"` // e.g. ["voice"]
	Bus        ServiceBus       `yaml:"bus,omitempty"`
}

// ServicePort maps a container port to a host port.
type ServicePort struct {
	Host      int    `yaml:"host"`
	Container int    `yaml:"container"`
	Protocol  string `yaml:"protocol,omitempty"` // tcp (default), udp
}

// ServiceResources defines resource constraints.
type ServiceResources struct {
	Memory string  `yaml:"memory,omitempty"` // e.g. "4G"
	CPUs   float64 `yaml:"cpus,omitempty"`   // e.g. 2.0
}

// ServiceVolume maps a host path to a container path.
type ServiceVolume struct {
	HostPath      string `yaml:"hostPath"`
	ContainerPath string `yaml:"containerPath"`
}

// ServiceHealth defines health check configuration.
type ServiceHealth struct {
	Endpoint    string `yaml:"endpoint"`
	Port        int    `yaml:"port"`
	Interval    string `yaml:"interval,omitempty"`    // e.g. "30s"
	Timeout     string `yaml:"timeout,omitempty"`     // e.g. "10s"
	Retries     int    `yaml:"retries,omitempty"`
	StartPeriod string `yaml:"startPeriod,omitempty"` // e.g. "60s"
}

// ServiceTool describes an HTTP tool endpoint exposed by the service.
type ServiceTool struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Endpoint    string `yaml:"endpoint"`
	Method      string `yaml:"method,omitempty"` // GET, POST, etc.
}

// ServiceBus defines bus integration for the service.
type ServiceBus struct {
	Advertise bool `yaml:"advertise,omitempty"`
}

// ─── Resource Parsing ───────────────────────────────────────────────────────────

// ParseMemoryBytes parses a human-readable memory string (e.g. "4G", "512M")
// into bytes.
func ParseMemoryBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSpace(s)
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "G") || strings.HasSuffix(s, "g"):
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "M") || strings.HasSuffix(s, "m"):
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "K") || strings.HasSuffix(s, "k"):
		multiplier = 1024
		s = s[:len(s)-1]
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid memory value: %s", s)
	}
	return n * multiplier, nil
}

// ParseDuration parses a duration string with support for "s", "m", "h" suffixes.
func ParseServiceDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// ─── Loader ─────────────────────────────────────────────────────────────────────

// serviceCRDDir returns the path to the service definitions directory.
func serviceCRDDir(root string) string {
	return filepath.Join(root, ".cog", "config", "services")
}

// LoadServiceCRD loads a single service CRD by name.
// Looks for {root}/.cog/config/services/{name}.service.yaml.
func LoadServiceCRD(root, name string) (*ServiceCRD, error) {
	path := filepath.Join(serviceCRDDir(root), name+".service.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load service CRD %q: %w", name, err)
	}

	var crd ServiceCRD
	if err := yaml.Unmarshal(data, &crd); err != nil {
		return nil, fmt.Errorf("parse service CRD %q: %w", name, err)
	}

	if crd.APIVersion != "cog.os/v1alpha1" || crd.Kind != "Service" {
		return nil, fmt.Errorf("service CRD %q: unexpected apiVersion=%q kind=%q",
			name, crd.APIVersion, crd.Kind)
	}

	if crd.Metadata.Name != name {
		return nil, fmt.Errorf("service CRD %q: metadata.name %q does not match filename",
			name, crd.Metadata.Name)
	}

	if crd.Spec.Image == "" {
		return nil, fmt.Errorf("service CRD %q: spec.image is required", name)
	}

	// Apply defaults
	if crd.Spec.Restart == "" {
		crd.Spec.Restart = "unless-stopped"
	}
	for i := range crd.Spec.Ports {
		if crd.Spec.Ports[i].Protocol == "" {
			crd.Spec.Ports[i].Protocol = "tcp"
		}
	}
	// Only apply health defaults when a health check is actually configured
	if crd.Spec.Health.Endpoint != "" && crd.Spec.Health.Port > 0 {
		if crd.Spec.Health.Interval == "" {
			crd.Spec.Health.Interval = "30s"
		}
		if crd.Spec.Health.Timeout == "" {
			crd.Spec.Health.Timeout = "10s"
		}
		if crd.Spec.Health.Retries == 0 {
			crd.Spec.Health.Retries = 3
		}
		if crd.Spec.Health.StartPeriod == "" {
			crd.Spec.Health.StartPeriod = "60s"
		}
	}

	return &crd, nil
}

// ListServiceCRDs loads all service CRDs from the services directory.
func ListServiceCRDs(root string) ([]ServiceCRD, error) {
	dir := serviceCRDDir(root)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list service CRDs: %w", err)
	}

	var crds []ServiceCRD
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".service.yaml") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".service.yaml")
		crd, err := LoadServiceCRD(root, name)
		if err != nil {
			return nil, err
		}
		crds = append(crds, *crd)
	}
	return crds, nil
}

// ─── Config Builders ────────────────────────────────────────────────────────────

// BuildContainerConfig converts a ServiceCRD into a DockerClient ContainerConfig.
func BuildContainerConfig(root string, crd *ServiceCRD) (ContainerConfig, error) {
	labels := ManagedLabels(crd.Metadata.Name)
	for k, v := range crd.Metadata.Labels {
		labels[k] = v
	}

	exposed := make(map[string]struct{})
	portBindings := make(map[string][]PortBinding)
	for _, p := range crd.Spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", p.Container, proto)
		exposed[key] = struct{}{}
		portBindings[key] = []PortBinding{{
			HostIP:   "127.0.0.1",
			HostPort: fmt.Sprintf("%d", p.Host),
		}}
	}

	var binds []string
	for _, v := range crd.Spec.Volumes {
		hostPath := v.HostPath
		// Resolve relative paths against workspace root
		if !filepath.IsAbs(hostPath) {
			hostPath = filepath.Join(root, hostPath)
		}
		// Ensure host directory exists
		if err := os.MkdirAll(hostPath, 0755); err != nil {
			return ContainerConfig{}, fmt.Errorf("create volume dir %s: %w", hostPath, err)
		}
		binds = append(binds, hostPath+":"+v.ContainerPath)
	}

	memBytes, err := ParseMemoryBytes(crd.Spec.Resources.Memory)
	if err != nil {
		return ContainerConfig{}, fmt.Errorf("parse memory: %w", err)
	}

	nanoCPUs := int64(crd.Spec.Resources.CPUs * 1e9)

	restartPolicy := RestartPolicy{Name: "no"}
	switch crd.Spec.Restart {
	case "always":
		restartPolicy = RestartPolicy{Name: "always"}
	case "unless-stopped":
		restartPolicy = RestartPolicy{Name: "unless-stopped"}
	case "on-failure":
		restartPolicy = RestartPolicy{Name: "on-failure", MaximumRetryCount: 5}
	}

	return ContainerConfig{
		Image:        crd.Spec.Image,
		Env:          crd.Spec.Env,
		Labels:       labels,
		ExposedPorts: exposed,
		Platform:     crd.Spec.Platform,
		HostConfig: HostConfig{
			PortBindings:  portBindings,
			Binds:         binds,
			Memory:        memBytes,
			NanoCPUs:      nanoCPUs,
			RestartPolicy: restartPolicy,
		},
	}, nil
}
