package preflight

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/network"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Options struct {
	RuntimeClass, StorageClass string
	Network                    network.Provider
}

type Result struct {
	Name   string
	Passed bool
	Detail string
}

func Check(ctx context.Context, c client.Client, o Options) []Result {
	results := []Result{}
	add := func(name string, e error) {
		if e == nil {
			results = append(results, Result{Name: name, Passed: true, Detail: "present"})
			return
		}
		results = append(results, Result{Name: name, Passed: false, Detail: e.Error()})
	}
	sandbox := &unstructured.Unstructured{}
	sandbox.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	err := c.Get(ctx, client.ObjectKey{Name: "sandboxes.agents.x-k8s.io"}, sandbox)
	if err != nil {
		err = errors.New("Agent Sandbox CRD sandboxes.agents.x-k8s.io is required")
	}
	add("agent-sandbox", err)
	name := "cilium-dns"
	if o.Network.Capabilities().ServiceProxy {
		name = "network-policy-dns"
	}
	add(name, o.Network.Check(ctx, c))
	runtime := &unstructured.Unstructured{}
	runtime.SetGroupVersionKind(schema.GroupVersionKind{Group: "node.k8s.io", Version: "v1", Kind: "RuntimeClass"})
	err = c.Get(ctx, client.ObjectKey{Name: o.RuntimeClass}, runtime)
	if err != nil {
		err = fmt.Errorf("RuntimeClass %s unavailable", o.RuntimeClass)
	}
	add("runtime", err)
	sc := &storagev1.StorageClass{}
	err = c.Get(ctx, client.ObjectKey{Name: o.StorageClass}, sc)
	if err != nil || sc.ReclaimPolicy == nil || *sc.ReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		err = fmt.Errorf("StorageClass %s must exist and reclaim Delete", o.StorageClass)
	} else {
		err = nil
	}
	add("storage", err)
	return results
}

func Run(ctx context.Context) error {
	config := ctrl.GetConfigOrDie()
	scheme := runtime.NewScheme()
	if e := clientgoscheme.AddToScheme(scheme); e != nil {
		return e
	}
	if e := storagev1.AddToScheme(scheme); e != nil {
		return e
	}
	c, e := client.New(config, client.Options{Scheme: scheme})
	if e != nil {
		return e
	}
	provider, e := network.FromEnvironment()
	if e != nil {
		return e
	}
	results := Check(ctx, c, Options{RuntimeClass: os.Getenv("RUNTIME_CLASS"), StorageClass: os.Getenv("STORAGE_CLASS"), Network: provider})
	return Report(os.Stdout, results)
}
func Report(w io.Writer, results []Result) error {
	failed := false
	for _, result := range results {
		state := "pass"
		if !result.Passed {
			state = "fail"
			failed = true
		}
		if _, e := fmt.Fprintf(w, "%s\t%s\t%s\n", state, result.Name, strings.ReplaceAll(result.Detail, "\n", " ")); e != nil {
			return e
		}
	}
	if failed {
		return errors.New("preflight failed")
	}
	return nil
}
