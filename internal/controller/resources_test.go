package controller

import (
	"strings"
	"testing"
)

func TestResourceProfiles(t *testing.T) {
	worker, proxy, e := (Config{}).resources()
	if e != nil {
		t.Fatal(e)
	}
	if worker.Limits.Memory().String() != "4Gi" || proxy.Limits.Memory().String() != "512Mi" {
		t.Fatal("workspace and proxy defaults should differ")
	}
	c := Config{WorkerResources: ResourceProfile{CPURequest: "750m", MemoryRequest: "1Gi", CPULimit: "2", MemoryLimit: "8Gi"}}
	worker, proxy, e = c.resources()
	if e != nil {
		t.Fatal(e)
	}
	if worker.Requests.Cpu().String() != "750m" || worker.Limits.Memory().String() != "8Gi" || proxy.Limits.Memory().String() != "512Mi" {
		t.Fatal("operator sizing not honored independently")
	}
	for _, p := range []ResourceProfile{{CPURequest: "garbage"}, {MemoryRequest: "0"}, {CPULimit: "-1"}, {CPURequest: "5"}, {MemoryRequest: "8Gi"}, {MemoryLimit: "0"}} {
		if _, _, e = (Config{WorkerResources: p}).resources(); e == nil {
			t.Fatal("invalid workspace sizing accepted", p)
		}
		if _, _, e = (Config{ProxyResources: p}).resources(); e == nil {
			t.Fatal("invalid proxy sizing accepted", p)
		}
	}
}

func TestOptionalProxyImageMustBePinned(t *testing.T) {
	c := Config{Namespace: "control", Owner: "owner", RuntimeClass: "gvisor", StorageClass: "delete", StorageSize: "1Gi", Image: "registry.example.com/worker@sha256:" + strings.Repeat("a", 64)}
	c.ProxyImage = "registry.example.com/proxy:latest"
	if c.Validate() == nil {
		t.Fatal("mutable proxy image accepted")
	}
	c.ProxyImage = "registry.example.com/proxy@sha256:" + strings.Repeat("b", 64)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.proxyImage() != c.ProxyImage {
		t.Fatal("dedicated proxy image ignored")
	}
}
