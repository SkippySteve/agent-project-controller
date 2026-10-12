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

func TestHelmNetworkPolicyProfile(t *testing.T) {
	if os.Getenv("APC_HELM_TEST") != "true" {
		t.Skip("set APC_HELM_TEST=true with Helm installed")
	}
	values := map[string]any{"controllerImage": "registry.example.com/controller@sha256:" + strings.Repeat("a", 64), "workspaceImage": "registry.example.com/workspace@sha256:" + strings.Repeat("b", 64), "config": map[string]any{"FORGEJO_ORIGIN": "https://git.example.com", "FORGEJO_OWNER": "operator", "PUBLIC_ORIGIN": "https://projects.example.com", "OIDC_ISSUER": "https://identity.example.com/realm", "OIDC_CLIENT_ID": "projects", "NETWORK_PROVIDER": "cilium"}}
	path := filepath.Join(t.TempDir(), "values.json")
	write := func() {
		b, _ := json.Marshal(values)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	render := func() ([]byte, error) {
		return exec.Command("helm", "template", "qualification", "../../charts/agent-project-controller", "-n", "test-control", "-f", path).CombinedOutput()
	}
	write()
	output, err := render()
	if err != nil {
		t.Fatalf("default provider: %v: %s", err, output)
	}
	if !bytes.Contains(output, []byte("ciliumnetworkpolicies")) || bytes.Contains(output, []byte("resources: [networkpolicies]")) {
		t.Fatal("default profile changed NetworkPolicy RBAC")
	}
	config := values["config"].(map[string]any)
	config["NETWORK_PROVIDER"] = "network-policy"
	for _, invalid := range []map[string]string{
		{"NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "false", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": `["10.0.0.0/8"]`},
		{"NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": ""},
		{"NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": "[]"},
		{"NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED": "true", "NETWORK_POLICY_CLUSTER_CIDRS_JSON": "{"},
	} {
		for key, value := range invalid {
			config[key] = value
		}
		write()
		if _, err := render(); err == nil {
			t.Fatalf("invalid network-policy values accepted: %v", invalid)
		}
	}
	config["NETWORK_PROVIDER"] = "calico"
	config["NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED"] = "true"
	config["NETWORK_POLICY_CLUSTER_CIDRS_JSON"] = `["10.0.0.0/8"]`
	write()
	if _, err := render(); err == nil {
		t.Fatal("unknown provider accepted")
	}
	config["NETWORK_PROVIDER"] = "network-policy"
	write()
	values["proxyGateway"] = map[string]any{"namespace": "edge", "selector": map[string]string{"app": "gateway"}, "port": 8443, "disableSNIFilter": false}
	write()
	if _, err := render(); err == nil {
		t.Fatal("network-policy gateway without disableSNIFilter accepted")
	}
	values["proxyGateway"] = map[string]any{"namespace": "edge", "selector": map[string]string{"app": "gateway"}, "port": 8443, "disableSNIFilter": true}
	write()
	output, err = render()
	if err != nil {
		t.Fatalf("experimental profile: %v: %s", err, output)
	}
	for _, want := range []string{"resources: [networkpolicies]", "resources: [nodes]", "NETWORK_PROVIDER: \"network-policy\"", "NETWORK_POLICY_ENFORCEMENT_ACKNOWLEDGED: \"true\"", `disableSNIFilter\":true`} {
		if !bytes.Contains(output, []byte(want)) {
			t.Fatalf("missing %s in %s", want, output)
		}
	}
	if bytes.Contains(output, []byte("ciliumnetworkpolicies")) || bytes.Contains(output, []byte("cilium.io")) {
		t.Fatal("experimental profile kept Cilium RBAC")
	}
}

func TestHelmGitHubHost(t *testing.T) {
	if os.Getenv("APC_HELM_TEST") != "true" {
		t.Skip("set APC_HELM_TEST=true with Helm installed")
	}
	config := map[string]any{"GIT_HOST": "github", "GITHUB_OWNER": "owner", "GITHUB_APP_ID": "12345", "GITHUB_APP_INSTALLATION_ID": "99", "PUBLIC_ORIGIN": "https://projects.example.com", "OIDC_ISSUER": "https://identity.example.com/realm", "OIDC_CLIENT_ID": "projects", "FORGEJO_ORIGIN": "", "FORGEJO_OWNER": ""}
	values := map[string]any{"controllerImage": "registry.example.com/controller@sha256:" + strings.Repeat("a", 64), "workspaceImage": "registry.example.com/workspace@sha256:" + strings.Repeat("b", 64), "config": config}
	path := filepath.Join(t.TempDir(), "values.json")
	write := func() {
		b, _ := json.Marshal(values)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	render := func() ([]byte, error) {
		return exec.Command("helm", "template", "qualification", "../../charts/agent-project-controller", "-n", "test-control", "-f", path).CombinedOutput()
	}
	write()
	output, err := render()
	if err != nil {
		t.Fatalf("github host: %v: %s", err, output)
	}
	for _, want := range []string{"GIT_HOST: \"github\"", "GITHUB_OWNER: \"owner\"", "GITHUB_APP_ID: \"12345\"", "GITHUB_APP_INSTALLATION_ID: \"99\"", "name: GITHUB_APP_PRIVATE_KEY_FILE", "value: /control-secrets/github-app-private-key"} {
		if !bytes.Contains(output, []byte(want)) {
			t.Fatalf("missing %s in %s", want, output)
		}
	}
	if bytes.Contains(output, []byte("FORGEJO_TOKEN_FILE")) || bytes.Contains(output, []byte("FORGEJO_ORIGIN")) || bytes.Contains(output, []byte("FORGEJO_OWNER")) {
		t.Fatalf("github host kept Forgejo settings: %s", output)
	}
	for _, invalid := range []map[string]string{
		{"GITHUB_APP_ID": ""},
		{"GITHUB_APP_ID": "0"},
		{"GITHUB_APP_INSTALLATION_ID": "01"},
		{"GIT_HOST": "gitlab"},
		{"FORGEJO_ORIGIN": "https://git.example.com"},
	} {
		saved := map[string]any{}
		for key, value := range invalid {
			saved[key] = config[key]
			config[key] = value
		}
		write()
		if _, err := render(); err == nil {
			t.Fatalf("invalid github values accepted: %v", invalid)
		}
		for key, value := range saved {
			config[key] = value
		}
	}
	config["GIT_HOST"] = "forgejo"
	config["FORGEJO_ORIGIN"] = "https://git.example.com"
	config["FORGEJO_OWNER"] = "operator"
	write()
	if _, err := render(); err == nil {
		t.Fatal("forgejo host accepted GitHub App settings")
	}
}
