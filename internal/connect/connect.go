package connect

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed model.py
var modelSession string

// ModelCommand runs the manual client in the worker. Credentials never cross
// the operator's terminal transport; the client reads the worker mount.
func ModelCommand() []string {
	return []string{"python3", "-c", modelSession}
}

// Command is the tool-independent shell command for a Ready workspace.
// Orca, T3, and Roamgate can run it when they can start a local process.
func Command(namespace, pod string) string {
	return fmt.Sprintf("kubectl -n %s exec -it %s -c workspace -- /bin/sh", namespace, pod)
}

func Pod(ctx context.Context, c client.Client, controlNamespace, workspace string) (*api.ProjectWorkspace, *corev1.Pod, error) {
	w := &api.ProjectWorkspace{}
	if e := c.Get(ctx, client.ObjectKey{Namespace: controlNamespace, Name: workspace}, w); e != nil {
		return nil, nil, e
	}
	if w.Status.Phase != "Ready" || w.Status.Namespace == "" {
		return w, nil, errors.New("workspace is not ready")
	}
	var pods corev1.PodList
	if e := c.List(ctx, &pods, client.InNamespace(w.Status.Namespace), client.MatchingLabels{api.OwnerLabel: string(w.UID), "projects.agentwork.dev/role": "worker"}); e != nil {
		return w, nil, e
	}
	if len(pods.Items) != 1 {
		return w, nil, errors.New("workspace does not have exactly one worker pod")
	}
	return w, &pods.Items[0], nil
}

func Exec(ctx context.Context, config *rest.Config, namespace, pod string, command []string, in io.Reader, out, errOut io.Writer, tty bool) error {
	if len(command) == 0 {
		return errors.New("command required")
	}
	if tty {
		if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
			state, err := term.MakeRaw(int(file.Fd()))
			if err != nil {
				return err
			}
			defer term.Restore(int(file.Fd()), state)
		}
	}
	clientset, e := kubernetes.NewForConfig(config)
	if e != nil {
		return e
	}
	request := clientset.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: "workspace", Command: command, Stdin: in != nil, Stdout: true, Stderr: true, TTY: tty}, scheme.ParameterCodec)
	executor, e := remotecommand.NewSPDYExecutor(config, "POST", request.URL())
	if e != nil {
		return e
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: in, Stdout: out, Stderr: errOut, Tty: tty})
}

func Run(ctx context.Context, c client.Client, config *rest.Config, controlNamespace, workspace string) error {
	_, pod, e := Pod(ctx, c, controlNamespace, workspace)
	if e != nil {
		return e
	}
	info, statErr := os.Stdin.Stat()
	tty := statErr == nil && info.Mode()&os.ModeCharDevice != 0
	fmt.Fprintf(os.Stderr, "%s\n", Command(pod.Namespace, pod.Name))
	return Exec(ctx, config, pod.Namespace, pod.Name, []string{"/bin/sh"}, os.Stdin, os.Stdout, os.Stderr, tty)
}

// ExecCommand is the non-interactive command T3, Roamgate, or Orca can start.
func ExecCommand(namespace, pod string, command []string) (string, error) {
	if len(command) == 0 {
		return "", errors.New("command required")
	}
	return fmt.Sprintf("kubectl -n %s exec %s -c workspace -- %s", namespace, pod, strings.Join(command, " ")), nil
}
