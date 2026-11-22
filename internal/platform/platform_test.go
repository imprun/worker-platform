package platform

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRouteOverwritesActorTarget(t *testing.T) {
	w, profile := ateExample(t)
	plan, err := BuildATEPlan(w, profile)
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Rules []struct {
			Matches []struct {
				Method string
				Path   struct{ Type, Value string }
			}
			Filters []struct {
				Type                  string
				RequestHeaderModifier struct {
					Set []struct{ Name, Value string }
				}
			}
		}
	}
	if err := json.Unmarshal(plan.Route.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Rules) != 1 || len(spec.Rules[0].Matches) != 1 || len(spec.Rules[0].Filters) != 1 {
		t.Fatalf("unexpected route rules: %s", plan.Route.Spec)
	}
	match := spec.Rules[0].Matches[0]
	filter := spec.Rules[0].Filters[0]
	if match.Method != "POST" || match.Path.Type != "Exact" || match.Path.Value != w.Spec.Trigger.Path ||
		filter.Type != "RequestHeaderModifier" || len(filter.RequestHeaderModifier.Set) != 1 {
		t.Fatalf("unexpected trigger: %s", plan.Route.Spec)
	}
	header := filter.RequestHeaderModifier.Set[0]
	if header.Name != "ate-target-actor" || header.Value != profile.Spec.Atespace+"/"+plan.Actor.GetMetadata().GetName() {
		t.Fatalf("unexpected Actor target: %+v", header)
	}
}

func TestWorkerRejectsAmbiguousManifests(t *testing.T) {
	data, err := os.ReadFile("../../examples/echo/ate-worker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	for name, changed := range map[string]string{
		"unknown key":               strings.Replace(manifest, "  profile:", "  profiles:", 1),
		"second document":           manifest + "\n---\n{}\n",
		"unpinned image":            strings.Replace(manifest, "@sha256:"+strings.Repeat("1", 64), ":latest", 1),
		"wildcard":                  strings.Replace(manifest, "echo.example.test", "'*.example.test'", 1),
		"retired version":           strings.Replace(manifest, WorkerVersion, "workerplatform.imprun.dev/v1alpha1", 1),
		"inline execution settings": strings.Replace(manifest, "spec:\n", "spec:\n  atespace: workers\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadWorker(strings.NewReader(changed)); err == nil {
				t.Fatal("manifest was accepted")
			}
		})
	}
}

func TestRoutePatchChecksObjectIdentityAndOwnership(t *testing.T) {
	w, profile := ateExample(t)
	p, err := BuildATEPlan(w, profile)
	if err != nil {
		t.Fatal(err)
	}
	current := p.Route
	current.Metadata.UID, current.Metadata.Generation = "uid", 9
	patch, err := routePatch(p.Route, &current)
	if err != nil || len(patch) != 0 {
		t.Fatalf("unchanged spec should be a no-op: %s %v", patch, err)
	}
	current.Spec = json.RawMessage(`{"rules":[]}`)
	patch, err = routePatch(p.Route, &current)
	if err != nil || !bytes.Contains(patch, []byte(`"path":"/metadata/uid","value":"uid"`)) ||
		!bytes.Contains(patch, []byte(`"path":"/metadata/generation","value":9`)) ||
		!bytes.Contains(patch, []byte(`"path":"/metadata/annotations/workerplatform.imprun.dev~1owner"`)) {
		t.Fatalf("missing a compare-and-swap guard: %s %v", patch, err)
	}
}
