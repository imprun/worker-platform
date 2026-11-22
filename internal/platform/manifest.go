package platform

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const WorkerVersion = "workerplatform.imprun.dev/v1alpha1"

type Worker struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   ObjectName `yaml:"metadata"`
	Spec       WorkerSpec `yaml:"spec"`
}

type ObjectName struct {
	Name      string `yaml:"name" json:"name"`
	Namespace string `yaml:"namespace" json:"namespace"`
}

type WorkerSpec struct {
	Atespace      string      `yaml:"atespace"`
	Image         string      `yaml:"image"`
	CPU           string      `yaml:"cpu"`
	Memory        string      `yaml:"memory"`
	EgressGateway string      `yaml:"egressGateway"`
	Trigger       HTTPTrigger `yaml:"trigger"`
}

type HTTPTrigger struct {
	Hostname string     `yaml:"hostname"`
	Path     string     `yaml:"path"`
	Gateway  GatewayRef `yaml:"gateway"`
	Router   RouterRef  `yaml:"router"`
}

type GatewayRef struct {
	ObjectName  `yaml:",inline"`
	SectionName string `yaml:"sectionName"`
}

type RouterRef struct {
	ObjectName `yaml:",inline"`
	Port       int `yaml:"port"`
}

var (
	labelPattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	imagePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9./_-]*(:[0-9]+)?/[a-z0-9][a-z0-9./_-]*@sha256:[a-f0-9]{64}$`)
	cpuPattern    = regexp.MustCompile(`^[1-9][0-9]{0,7}m?$`)
	memoryPattern = regexp.MustCompile(`^[1-9][0-9]{0,7}(Mi|Gi)$`)
	pathPattern   = regexp.MustCompile(`^/[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*$`)
)

func ReadWorker(r io.Reader) (Worker, error) {
	const maxSize = 64 * 1024
	limited := &io.LimitedReader{R: r, N: maxSize + 1}
	decoder := yaml.NewDecoder(limited)
	decoder.KnownFields(true)
	var w Worker
	if err := decoder.Decode(&w); err != nil {
		return Worker{}, fmt.Errorf("decode HTTPWorker: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || limited.N == 0 {
		return Worker{}, errors.New("expected one HTTPWorker document of at most 64 KiB")
	}
	if err := w.Validate(); err != nil {
		return Worker{}, err
	}
	return w, nil
}

func (w Worker) Validate() error {
	if w.APIVersion != WorkerVersion || w.Kind != "HTTPWorker" {
		return fmt.Errorf("expected %s HTTPWorker", WorkerVersion)
	}
	if err := checkLabels(w.Metadata.Name, w.Metadata.Namespace, w.Spec.Atespace); err != nil {
		return fmt.Errorf("worker identity: %w", err)
	}
	if len(w.Metadata.Name) > 40 {
		return errors.New("worker name must be at most 40 characters")
	}
	if !imagePattern.MatchString(w.Spec.Image) || len(w.Spec.Image) > 512 {
		return errors.New("spec.image must be a repository reference pinned to a SHA-256 digest")
	}
	if !cpuPattern.MatchString(w.Spec.CPU) || !memoryPattern.MatchString(w.Spec.Memory) {
		return errors.New("spec.cpu must be positive cores or millicores and spec.memory must be positive Mi or Gi")
	}
	if w.Spec.EgressGateway != "" && !labelPattern.MatchString(w.Spec.EgressGateway) {
		return errors.New("spec.egressGateway must be a DNS label")
	}
	t := w.Spec.Trigger
	if len(t.Hostname) > 253 || !strings.Contains(t.Hostname, ".") || checkLabels(strings.Split(t.Hostname, ".")...) != nil {
		return errors.New("spec.trigger.hostname must be an exact DNS hostname with at least two labels")
	}
	if len(t.Path) > 253 || !pathPattern.MatchString(t.Path) || t.Path == "/healthz" || t.Path == "/readyz" {
		return errors.New("spec.trigger.path must be an application path made of unescaped segments")
	}
	if err := checkLabels(t.Gateway.Name, t.Gateway.Namespace, t.Gateway.SectionName, t.Router.Name, t.Router.Namespace); err != nil {
		return fmt.Errorf("gateway and router references: %w", err)
	}
	if t.Router.Port < 1 || t.Router.Port > 65535 {
		return errors.New("spec.trigger.router.port must be between 1 and 65535")
	}
	return nil
}

func checkLabels(values ...string) error {
	for _, value := range values {
		if !labelPattern.MatchString(value) {
			return fmt.Errorf("%q must be a DNS label", value)
		}
	}
	return nil
}
