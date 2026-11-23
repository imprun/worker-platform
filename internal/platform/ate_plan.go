package platform

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/imprun/worker-platform/internal/apppackage"
	ate "go.ate.dev/substrate/api/ateapi/v1"
)

const (
	PoolIDLabel    = "workerplatform.imprun.dev/pool-id"
	actorReadyPath = "/readyz"
)

type WorkerProfile struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   ObjectName        `yaml:"metadata"`
	Spec       WorkerProfileSpec `yaml:"spec"`
}

type WorkerProfileSpec struct {
	Atespace      string       `yaml:"atespace" json:"atespace"`
	WorkerPool    ObjectName   `yaml:"workerPool" json:"workerPool"`
	PoolID        string       `yaml:"poolID" json:"poolID"`
	Architecture  string       `yaml:"architecture" json:"architecture"`
	SandboxClass  string       `yaml:"sandboxClass" json:"sandboxClass"`
	CPU           string       `yaml:"cpu" json:"cpu"`
	Memory        string       `yaml:"memory" json:"memory"`
	Egress        []EgressRule `yaml:"egress" json:"egress"`
	RuntimeImage  string       `yaml:"runtimeImage,omitempty" json:"runtimeImage,omitempty"`
	AppRegistryCA string       `yaml:"appRegistryCA,omitempty" json:"appRegistryCA,omitempty"`
}

type EgressRule struct {
	Protocol string `yaml:"protocol" json:"protocol"`
	Hostname string `yaml:"hostname" json:"hostname"`
	Port     int32  `yaml:"port" json:"port"`
}

func ReadWorkerProfile(r io.Reader) (WorkerProfile, error) {
	var profile WorkerProfile
	if err := readManifest(r, "WorkerProfile", &profile); err != nil {
		return WorkerProfile{}, err
	}
	if err := profile.Validate(); err != nil {
		return WorkerProfile{}, err
	}
	return profile, nil
}

func (p WorkerProfile) Validate() error {
	if p.APIVersion != WorkerVersion || p.Kind != "WorkerProfile" {
		return fmt.Errorf("expected %s WorkerProfile", WorkerVersion)
	}
	s := p.Spec
	if err := checkLabels(p.Metadata.Name, p.Metadata.Namespace, s.Atespace, s.WorkerPool.Name, s.WorkerPool.Namespace, s.PoolID); err != nil {
		return fmt.Errorf("profile identity, atespace or pool: %w", err)
	}
	if s.SandboxClass != "gvisor" || (s.Architecture != "arm64" && s.Architecture != "amd64") {
		return errors.New("WorkerProfile requires gvisor with an arm64 or amd64 pool")
	}
	if !cpuPattern.MatchString(s.CPU) || !memoryPattern.MatchString(s.Memory) || s.resources().GetCpuMillicores() > 999999 {
		return errors.New("profile CPU must be positive and below 1000 cores, and memory must be positive Mi or Gi")
	}
	if s.RuntimeImage != "" && (!imagePattern.MatchString(s.RuntimeImage) || len(s.RuntimeImage) > 512) {
		return errors.New("profile runtimeImage must be a repository reference pinned to a SHA-256 digest")
	}
	if s.AppRegistryCA != "" {
		if err := apppackage.AppendRegistryCA(x509.NewCertPool(), s.AppRegistryCA); err != nil {
			return fmt.Errorf("profile appRegistryCA: %w", err)
		}
	}
	if len(s.Egress) > 256 {
		return errors.New("profile must contain at most 256 egress rules")
	}
	seen := make(map[string]bool, len(s.Egress))
	for _, rule := range s.Egress {
		if rule.Port < 1 || rule.Port > 65535 || len(rule.Hostname) > 253 ||
			net.ParseIP(rule.Hostname) != nil || checkLabels(strings.Split(rule.Hostname, ".")...) != nil {
			return fmt.Errorf("invalid egress destination %q on port %d", rule.Hostname, rule.Port)
		}
		if rule.Protocol != "http" && rule.Protocol != "tlsPassthrough" {
			return fmt.Errorf("unsupported egress protocol %q", rule.Protocol)
		}
		target := net.JoinHostPort(rule.Hostname, strconv.Itoa(int(rule.Port)))
		if seen[target] {
			return fmt.Errorf("duplicate egress destination %s", target)
		}
		seen[target] = true
	}
	return nil
}

// The manifest's quantity format bounds both values before conversion.
func (p WorkerProfileSpec) resources() *ate.Resources {
	cpu, _ := strconv.ParseInt(strings.TrimSuffix(p.CPU, "m"), 10, 64)
	if !strings.HasSuffix(p.CPU, "m") {
		cpu *= 1000
	}
	memory, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSuffix(p.Memory, "Mi"), "Gi"), 10, 64)
	shift := 20
	if strings.HasSuffix(p.Memory, "Gi") {
		shift = 30
	}
	return &ate.Resources{CpuMillicores: cpu, MemoryBytes: memory << shift}
}

func (p WorkerProfileSpec) policy() *ate.EgressPolicy {
	rules := slices.Clone(p.Egress)
	slices.SortFunc(rules, func(a, b EgressRule) int {
		if order := strings.Compare(a.Hostname, b.Hostname); order != 0 {
			return order
		}
		return int(a.Port - b.Port)
	})
	policy := &ate.EgressPolicy{Metadata: &ate.ResourceMetadata{Atespace: p.Atespace, Name: "default"}}
	for _, rule := range rules {
		hosts := []string{rule.Hostname}
		ports := &ate.Ports{Numbers: []int32{rule.Port}}
		if rule.Protocol == "http" {
			policy.Rules = append(policy.Rules, &ate.EgressRule{Http: &ate.HttpRule{Hostnames: hosts, Ports: ports}})
		} else {
			policy.Rules = append(policy.Rules, &ate.EgressRule{TlsPassthrough: &ate.TlsPassthroughRule{Hostnames: hosts, Ports: ports}})
		}
	}
	return policy
}

type ATEPlan struct {
	Revision string
	Template *ate.ActorTemplate
	Actor    *ate.Actor
	Policy   *ate.EgressPolicy
	Route    Route
}

func BuildATEPlan(w Worker, p WorkerProfile) (ATEPlan, error) {
	return buildATEPlan(w, p, "ate:"+w.Metadata.Namespace+"/"+w.Metadata.Name)
}

func buildATEPlan(w Worker, p WorkerProfile, owner string) (ATEPlan, error) {
	if err := w.Validate(); err != nil {
		return ATEPlan{}, err
	}
	if err := p.Validate(); err != nil {
		return ATEPlan{}, err
	}
	if p.Metadata.Namespace != w.Metadata.Namespace || p.Metadata.Name != w.Spec.Profile {
		return ATEPlan{}, errors.New("WorkerProfile name and namespace must match the worker's profile reference")
	}
	container, err := workerContainer(w.Spec, p.Spec)
	if err != nil {
		return ATEPlan{}, err
	}
	template := &ate.ActorTemplate{
		WorkerSelector: &ate.Selector{MatchLabels: map[string]string{PoolIDLabel: p.Spec.PoolID}},
		Containers:     []*ate.Container{container},
		Resources:      &ate.ResourceRequirements{Limits: p.Spec.resources()},
		SnapshotConfig: &ate.SnapshotConfig{
			PreferredFidelity: ate.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			GoldenSnapshotConfig: &ate.GoldenSnapshotConfig{
				Mode: ate.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED,
			},
		},
	}
	policy := p.Spec.policy()
	identity, err := json.Marshal(struct {
		Owner        string
		WorkerPool   ObjectName
		Architecture string
		SandboxClass string
		Template     *ate.ActorTemplate
		Policy       *ate.EgressPolicy
	}{owner, p.Spec.WorkerPool, p.Spec.Architecture, p.Spec.SandboxClass, template, policy})
	if err != nil {
		return ATEPlan{}, err
	}
	sum := sha256.Sum256(identity)
	name := fmt.Sprintf("wp-%s-%x", w.Metadata.Name, sum[:9])
	template.Metadata = &ate.ResourceMetadata{Atespace: p.Spec.Atespace, Name: name}
	actor := &ate.Actor{
		Metadata:      &ate.ResourceMetadata{Atespace: p.Spec.Atespace, Name: name},
		ActorTemplate: &ate.ObjectRef{Atespace: p.Spec.Atespace, Name: name},
	}
	route, err := buildRoute(w, owner, p.Spec.Atespace+"/"+name)
	return ATEPlan{Revision: fmt.Sprintf("%x", sum), Template: template, Actor: actor, Policy: policy, Route: route}, err
}

func workerContainer(worker WorkerSpec, profile WorkerProfileSpec) (*ate.Container, error) {
	container := &ate.Container{
		Name:  "worker",
		Image: worker.Image,
		WakeupProbe: &ate.WakeupProbe{
			HttpGet:        &ate.HttpGetAction{Path: actorReadyPath, Port: 80},
			TimeoutSeconds: 30,
		},
	}
	if worker.AppPackage == "" {
		return container, nil
	}
	if profile.RuntimeImage == "" {
		return nil, errors.New("app packages require a WorkerProfile runtimeImage")
	}
	ref, err := apppackage.ParseReference(worker.AppPackage)
	if err != nil {
		return nil, err
	}
	allowed := slices.ContainsFunc(profile.Egress, func(rule EgressRule) bool {
		return rule.Protocol == "tlsPassthrough" && rule.Hostname == ref.Host && rule.Port == ref.Port
	})
	if !allowed {
		return nil, fmt.Errorf("app registry %s:%d requires a tlsPassthrough rule in WorkerProfile", ref.Host, ref.Port)
	}
	container.Image = profile.RuntimeImage
	container.Env = []*ate.EnvVar{{Name: apppackage.ReferenceEnv, Value: ref.String()}}
	if profile.AppRegistryCA != "" {
		container.Env = append(container.Env, &ate.EnvVar{Name: apppackage.RegistryCAEnv, Value: profile.AppRegistryCA})
	}
	return container, nil
}
