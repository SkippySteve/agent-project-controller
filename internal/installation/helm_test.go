package installation

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestChartSchemaMatchesSource(t *testing.T) {
	source, err := os.ReadFile("../../config/crd/projectworkspaces.yaml")
	if err != nil {
		t.Fatal(err)
	}
	chart, err := os.ReadFile("../../charts/agent-project-controller/crds/projectworkspaces.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, chart) {
		t.Fatal("packaged CRD differs from controller schema")
	}
}
func TestHelmPackage(t *testing.T) {
	if os.Getenv("APC_HELM_TEST") != "true" {
		t.Skip("set APC_HELM_TEST=true with Helm installed")
	}
	values := map[string]any{"controllerImage": "registry.example.com/controller@sha256:" + strings.Repeat("a", 64), "workspaceImage": "registry.example.com/workspace@sha256:" + strings.Repeat("b", 64), "config": map[string]any{"FORGEJO_ORIGIN": "https://git.example.com", "FORGEJO_OWNER": "operator", "PUBLIC_ORIGIN": "https://projects.example.com", "OIDC_ISSUER": "https://identity.example.com/realm", "OIDC_CLIENT_ID": "projects"}}
	path := filepath.Join(t.TempDir(), "values.json")
	write := func() {
		b, _ := json.Marshal(values)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	render := func() ([]byte, error) {
		return exec.Command("helm", "template", "qualification", "../../charts/agent-project-controller", "-n", "test-control", "-f", path, "--include-crds").CombinedOutput()
	}
	output, err := render()
	if err != nil {
		t.Fatalf("render: %v: %s", err, output)
	}
	for _, want := range []string{"kind: CustomResourceDefinition", "kind: Deployment", "CONTROL_NAMESPACE: \"test-control\"", "namespace: test-control", "name: test-control-qualification"} {
		if !bytes.Contains(output, []byte(want)) {
			t.Fatalf("missing %s", want)
		}
	}
	if bytes.Contains(output, []byte("kind: Secret")) || bytes.Contains(output, []byte("name: API_SECRET_FILE")) {
		t.Fatal("default chart creates credentials or enables automation API")
	}
	if bytes.Contains(output, []byte("kind: HTTPRoute")) {
		t.Fatal("default chart requires a gateway")
	}
	values["controllerScheduling"] = map[string]any{"nodeSelector": map[string]string{"pool": "control"}}
	values["workspaceScheduling"] = map[string]any{"nodeSelector": map[string]string{"pool": "agents"}}
	values["proxyScheduling"] = map[string]any{"nodeSelector": map[string]string{"pool": "proxies"}}
	values["controllerDNS"] = map[string]any{"policy": "None", "config": map[string]any{"nameservers": []string{"10.96.0.10"}}}
	values["proxyGateway"] = map[string]any{"namespace": "edge", "selector": map[string]string{"app": "gateway"}, "port": 9443}
	values["routing"] = map[string]any{"enabled": true, "hostnames": []string{"projects.example.com"}, "parentRefs": []map[string]string{{"name": "edge-gateway", "namespace": "edge"}}}
	write()
	output, err = render()
	if err != nil {
		t.Fatalf("custom cluster render: %v: %s", err, output)
	}
	for _, want := range []string{"kind: HTTPRoute", "name: edge-gateway", "projects.example.com", "pool: control", "dnsPolicy: \"None\"", "10.96.0.10", "WORKSPACE_SCHEDULING_JSON:", "PROXY_SCHEDULING_JSON:", `\"pool\":\"agents\"`, `\"port\":9443`} {
		if !bytes.Contains(output, []byte(want)) {
			t.Fatalf("custom values missing %s: %s", want, output)
		}
	}
	for _, invalid := range []map[string]any{
		{"workspaceScheduling": map[string]any{"hostNetwork": true}},
		{"proxyGateway": map[string]any{"namespace": "edge", "selector": map[string]string{"app": "gateway"}, "port": 0}},
		{"controllerDNS": map[string]any{"policy": "None", "config": map[string]any{}}},
		{"routing": map[string]any{"enabled": true, "hostnames": []string{}, "parentRefs": []any{}}},
	} {
		saved := map[string]any{}
		for key, value := range invalid {
			saved[key] = values[key]
			values[key] = value
		}
		write()
		if _, err := render(); err == nil {
			t.Fatalf("invalid cluster values accepted: %v", invalid)
		}
		for key, value := range saved {
			values[key] = value
		}
	}
	values["config"].(map[string]any)["PROXY_GATEWAY_JSON"] = `{"namespace":"other"}`
	write()
	if _, err := render(); err == nil {
		t.Fatal("conflicting gateway configuration accepted")
	}
	delete(values["config"].(map[string]any), "PROXY_GATEWAY_JSON")
	values["automationAPI"] = map[string]any{"enabled": true}
	write()
	output, err = render()
	if err != nil || !bytes.Contains(output, []byte("name: API_SECRET_FILE")) {
		t.Fatal("optional automation secret not mounted")
	}
	values["config"].(map[string]any)["CONTROL_NAMESPACE"] = "other-control"
	write()
	if _, err = render(); err == nil {
		t.Fatal("config overrides the release control namespace")
	}
	delete(values["config"].(map[string]any), "CONTROL_NAMESPACE")
	values["controllerImage"] = "registry.example.com/controller:latest"
	write()
	if _, err = render(); err == nil {
		t.Fatal("mutable image accepted")
	}
}
