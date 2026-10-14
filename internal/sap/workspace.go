package sap

import (
	"context"
	"strings"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	apisap "git.cluster.aimingupward.com/steve/agent-project-controller/pkg/sap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Workspaces publishes ProjectWorkspace resources through SAP.
// The catalog namespace is the controller namespace. Each item's namespace is
// the sandbox namespace recorded on the workspace, once one exists.
type Workspaces struct {
	Client    client.Client
	Namespace string
}

func (w *Workspaces) List(ctx context.Context) ([]apisap.Sandbox, error) {
	var list api.ProjectWorkspaceList
	if err := w.Client.List(ctx, &list, client.InNamespace(w.Namespace)); err != nil {
		return nil, err
	}
	items := make([]apisap.Sandbox, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, Convert(&list.Items[i]))
	}
	return items, nil
}

func (w *Workspaces) Get(ctx context.Context, namespace, name string) (apisap.Sandbox, error) {
	item := &api.ProjectWorkspace{}
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, item); err != nil {
		if apierrors.IsNotFound(err) {
			return apisap.Sandbox{}, apisap.ErrNotFound
		}
		return apisap.Sandbox{}, err
	}
	sandbox := Convert(item)
	if sandbox.Namespace != namespace {
		return apisap.Sandbox{}, apisap.ErrNotFound
	}
	return sandbox, nil
}

// Convert maps a project workspace into a SAP sandbox. It copies no credentials.
func Convert(workspace *api.ProjectWorkspace) apisap.Sandbox {
	namespace := workspace.Status.Namespace
	if namespace == "" {
		namespace = workspace.Namespace
	}
	phase, activity := apisap.PhasePending, apisap.ActivityIdle
	switch workspace.Status.Phase {
	case "Ready":
		phase = apisap.PhaseReady
	case "Paused":
		phase = apisap.PhasePaused
	case "Blocked":
		phase, activity = apisap.PhaseBlocked, apisap.ActivityBlocked
	case "Removing", "Removed":
		phase = apisap.PhaseTerminating
	case "Preparing", "Pausing":
		phase, activity = apisap.PhasePending, apisap.ActivityWorking
	}
	return apisap.Sandbox{
		ID:        workspace.Name,
		Namespace: namespace,
		Name:      workspace.Name,
		Labels:    map[string]string{"projects.agentwork.dev/workspace": workspace.Name},
		Phase:     phase,
		Activity:  activity,
		Project:   workspace.Spec.Repository,
		Owner:     repositoryOwner(workspace.Spec.Repository),
		Channels:  []string{apisap.ChannelTerminal},
	}
}

func repositoryOwner(repository string) string {
	owner, _, ok := strings.Cut(repository, "/")
	if !ok {
		return ""
	}
	return owner
}
