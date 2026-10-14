package sap

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/connect"
	apisap "git.cluster.aimingupward.com/steve/agent-project-controller/pkg/sap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StreamFunc runs one fixed command and copies its terminal until it returns.
type StreamFunc func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes remotecommand.TerminalSizeQueue) error

// PodExec attaches a SAP terminal to the workspace container through the
// Kubernetes API. The command is always /bin/sh. Callers cannot replace it.
type PodExec struct {
	Client           client.Client
	ControlNamespace string
	Stream           StreamFunc
}

// NewPodExec streams the shell with the controller's Kubernetes credentials.
func NewPodExec(c client.Client, config *rest.Config, controlNamespace string) *PodExec {
	return &PodExec{Client: c, ControlNamespace: controlNamespace, Stream: KubernetesStream(config)}
}

// Attach implements sap.AttachFunc.
func (p *PodExec) Attach(ctx context.Context, sandbox apisap.Sandbox, channel string) (apisap.Session, error) {
	if channel != apisap.ChannelTerminal {
		return nil, apisap.ErrChannel
	}
	if sandbox.Phase != apisap.PhaseReady {
		return nil, apisap.ErrNotReady
	}
	_, pod, err := connect.Pod(ctx, p.Client, p.ControlNamespace, sandbox.Name)
	if err != nil {
		switch err.Error() {
		case "workspace is not ready":
			return nil, apisap.ErrNotReady
		case "workspace does not have exactly one worker pod":
			return nil, apisap.ErrUnavailable
		default:
			return nil, apisap.ErrUnavailable
		}
	}
	if pod.Namespace != sandbox.Namespace {
		return nil, apisap.ErrNotFound
	}
	command := []string{"/bin/sh"}
	return startStream(ctx, func(ctx context.Context, stdin io.Reader, stdout io.Writer, sizes remotecommand.TerminalSizeQueue) error {
		return p.Stream(ctx, pod.Namespace, pod.Name, "workspace", command, stdin, stdout, sizes)
	})
}

// KubernetesStream is the SPDY exec implementation used for real sandboxes.
func KubernetesStream(config *rest.Config) StreamFunc {
	return func(ctx context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout io.Writer, sizes remotecommand.TerminalSizeQueue) error {
		if config == nil || len(command) != 1 || command[0] != "/bin/sh" || container != "workspace" {
			return apisap.ErrUnavailable
		}
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			return err
		}
		request := clientset.CoreV1().RESTClient().Post().Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			TTY:       true,
		}, scheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, request.URL())
		if err != nil {
			return err
		}
		return executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Tty: true, TerminalSizeQueue: sizes})
	}
}

type streamSession struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	sizes  chan remotecommand.TerminalSize
	done   chan struct{}
	once   sync.Once
	cancel context.CancelFunc
}

func startStream(parent context.Context, run func(context.Context, io.Reader, io.Writer, remotecommand.TerminalSizeQueue) error) (*streamSession, error) {
	ctx, cancel := context.WithCancel(parent)
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	session := &streamSession{
		reader: outReader,
		writer: inWriter,
		sizes:  make(chan remotecommand.TerminalSize, 1),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	session.sizes <- remotecommand.TerminalSize{Width: 80, Height: 24}
	go func() {
		err := run(ctx, inReader, outWriter, session)
		_ = outWriter.CloseWithError(cleanStreamErr(err))
	}()
	return session, nil
}

func cleanStreamErr(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (s *streamSession) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s *streamSession) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *streamSession) Resize(cols, rows uint16) error {
	size := remotecommand.TerminalSize{Width: cols, Height: rows}
	select {
	case <-s.done:
		return io.ErrClosedPipe
	default:
	}
	select {
	case s.sizes <- size:
		return nil
	default:
		select {
		case <-s.sizes:
		default:
		}
		select {
		case s.sizes <- size:
		default:
		}
		return nil
	}
}
func (s *streamSession) Close() error {
	s.once.Do(func() {
		s.cancel()
		close(s.done)
		_ = s.writer.Close()
	})
	return nil
}
func (s *streamSession) Next() *remotecommand.TerminalSize {
	select {
	case <-s.done:
		return nil
	case size, ok := <-s.sizes:
		if !ok {
			return nil
		}
		copied := size
		return &copied
	}
}
