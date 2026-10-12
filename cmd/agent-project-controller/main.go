package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	api "git.cluster.aimingupward.com/steve/agent-project-controller/api/v1alpha1"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/bootstrap"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/connect"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/controller"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/forgejo"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/network"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/preflight"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/proxy"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/telemetry"
	"git.cluster.aimingupward.com/steve/agent-project-controller/internal/web"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func env(k, fallback string) string {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	return v
}
func profile(prefix string) controller.ResourceProfile {
	return controller.ResourceProfile{CPURequest: os.Getenv(prefix + "_CPU_REQUEST"), MemoryRequest: os.Getenv(prefix + "_MEMORY_REQUEST"), CPULimit: os.Getenv(prefix + "_CPU_LIMIT"), MemoryLimit: os.Getenv(prefix + "_MEMORY_LIMIT")}
}
func secret(k string) (string, error) {
	p := os.Getenv(k)
	if p == "" {
		return "", fmt.Errorf("%s file path required", k)
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return "", fmt.Errorf("unable to read %s", k)
	}
	return strings.TrimSpace(string(b)), nil
}
func run(ctx context.Context) error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "bootstrap":
			return bootstrap.Run(ctx)
		case "credential":
			return bootstrap.Credential()
		case "proxy":
			return proxy.Run(ctx)
		case "preflight":
			return preflight.Run(ctx)
		case "connect":
			if len(os.Args) != 3 {
				return errors.New("usage: agent-project-controller connect <workspace>")
			}
			return connectCLI(ctx, os.Args[2], []string{"/bin/sh"})
		case "model":
			if len(os.Args) != 3 {
				return errors.New("usage: agent-project-controller model <workspace>")
			}
			return connectCLI(ctx, os.Args[2], connect.ModelCommand())
		case "exec":
			if len(os.Args) < 5 || os.Args[3] != "--" {
				return errors.New("usage: agent-project-controller exec <workspace> -- <command>")
			}
			return connectCLI(ctx, os.Args[2], os.Args[4:])
		case "controller":
		default:
			return errors.New("unknown command")
		}
	}
	ns := env("CONTROL_NAMESPACE", "agent-project-controller")
	token, e := secret("FORGEJO_TOKEN_FILE")
	if e != nil {
		return e
	}
	password := ""
	if os.Getenv("API_SECRET_FILE") != "" {
		password, e = secret("API_SECRET_FILE")
		if e != nil {
			return e
		}
	}
	host, e := forgejo.New(os.Getenv("FORGEJO_ORIGIN"), os.Getenv("FORGEJO_OWNER"), token)
	if e != nil {
		return e
	}
	shutdown, e := telemetry.Setup(ctx, os.Getenv("OTEL_ENABLED") == "true")
	if e != nil {
		slog.Warn("optional telemetry unavailable; controller enforcement continues")
		shutdown = func(context.Context) error { return nil }
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = shutdown(c)
	}()
	scheme := runtime.NewScheme()
	if e = clientgoscheme.AddToScheme(scheme); e != nil {
		return e
	}
	if e = api.AddToScheme(scheme); e != nil {
		return e
	}
	config := ctrl.GetConfigOrDie()
	manager, e := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, LeaderElection: true, LeaderElectionID: "agent-project-controller", LeaderElectionNamespace: ns, HealthProbeBindAddress: ":8081", Metrics: metricsserver.Options{BindAddress: "0"}, Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{&api.ProjectWorkspace{}: {Namespaces: map[string]cache.Config{ns: {}}}}}, Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&api.ProjectWorkspace{}}}}})
	if e != nil {
		return e
	}
	direct, e := client.New(config, client.Options{Scheme: scheme})
	if e != nil {
		return e
	}
	networkProvider := network.Cilium{ClusterDomain: env("CLUSTER_DOMAIN", "cluster.local"), DNSNamespace: env("DNS_SERVICE_NAMESPACE", "kube-system"), DNSService: env("DNS_SERVICE_NAME", "kube-dns"), DirectDNS: os.Getenv("DNS_DIRECT_ENDPOINTS") == "true"}
	if raw := os.Getenv("PROXY_GATEWAY_JSON"); raw != "" {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		var gateway network.Gateway
		if e := decoder.Decode(&gateway); e != nil {
			return errors.New("invalid proxy gateway configuration")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return errors.New("invalid trailing proxy gateway configuration")
		}
		networkProvider.Gateway = &gateway
	}
	if e := networkProvider.Validate(); e != nil {
		return e
	}
	workerScheduling, e := controller.ParseScheduling(os.Getenv("WORKSPACE_SCHEDULING_JSON"))
	if e != nil {
		return fmt.Errorf("workspace scheduling: %w", e)
	}
	proxyScheduling, e := controller.ParseScheduling(os.Getenv("PROXY_SCHEDULING_JSON"))
	if e != nil {
		return fmt.Errorf("proxy scheduling: %w", e)
	}
	reconciler := &controller.Reconciler{Client: direct, Host: host, Network: networkProvider, Config: controller.Config{Namespace: ns, ForgejoOrigin: host.Origin, Owner: host.Owner, Image: os.Getenv("WORKSPACE_IMAGE"), ProxyImage: os.Getenv("PROXY_IMAGE"), RuntimeClass: os.Getenv("RUNTIME_CLASS"), StorageClass: os.Getenv("STORAGE_CLASS"), StorageSize: env("STORAGE_SIZE", "10Gi"), WorkerResources: profile("WORKSPACE"), ProxyResources: profile("PROXY"), WorkerScheduling: workerScheduling, ProxyScheduling: proxyScheduling}}
	if e = reconciler.Config.Validate(); e != nil {
		return e
	}
	if e = reconciler.Setup(manager); e != nil {
		return e
	}
	if e = manager.AddHealthzCheck("healthz", healthz.Ping); e != nil {
		return e
	}
	if e = manager.AddReadyzCheck("readyz", healthz.Ping); e != nil {
		return e
	}
	server := &web.Server{Client: direct, Host: host, Namespace: ns, Owner: host.Owner, Password: password, Origin: os.Getenv("PUBLIC_ORIGIN"), AllowBasic: os.Getenv("ALLOW_STATIC_LOGIN") == "true"}
	if os.Getenv("OIDC_ISSUER") != "" {
		clientSecret, err := secret("OIDC_CLIENT_SECRET_FILE")
		if err != nil {
			return err
		}
		server.OIDC, e = web.NewOIDC(ctx, web.OIDCConfig{Issuer: os.Getenv("OIDC_ISSUER"), ClientID: os.Getenv("OIDC_CLIENT_ID"), ClientSecret: clientSecret, Origin: server.Origin, GroupsClaim: env("OIDC_GROUPS_CLAIM", "groups"), Scopes: web.CSV(env("OIDC_SCOPES", "profile,groups")), AllowedGroups: web.CSV(os.Getenv("OIDC_ALLOWED_GROUPS")), AllowedSubjects: web.CSV(os.Getenv("OIDC_ALLOWED_SUBJECTS"))})
		if e != nil {
			return e
		}
	}
	handler, e := server.Handler()
	if e != nil {
		return e
	}
	httpServer := &http.Server{Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	failures := make(chan error, 2)
	go func() { failures <- manager.Start(ctx) }()
	go func() {
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		failures <- err
	}()
	select {
	case <-ctx.Done():
	case e = <-failures:
	}
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(c)
	return e
}
func connectCLI(ctx context.Context, workspace string, command []string) error {
	config := ctrl.GetConfigOrDie()
	scheme := runtime.NewScheme()
	if e := clientgoscheme.AddToScheme(scheme); e != nil {
		return e
	}
	if e := api.AddToScheme(scheme); e != nil {
		return e
	}
	c, e := client.New(config, client.Options{Scheme: scheme})
	if e != nil {
		return e
	}
	_, pod, e := connect.Pod(ctx, c, env("CONTROL_NAMESPACE", "agent-project-controller"), workspace)
	if e != nil {
		return e
	}
	if len(command) == 1 && command[0] == "/bin/sh" {
		return connect.Run(ctx, c, config, env("CONTROL_NAMESPACE", "agent-project-controller"), workspace)
	}
	info, statErr := os.Stdin.Stat()
	tty := statErr == nil && info.Mode()&os.ModeCharDevice != 0
	if len(command) == 3 && command[0] == "python3" && command[1] == "-c" {
		fmt.Fprintf(os.Stderr, "Connecting to %s\n", workspace)
	} else {
		line, e := connect.ExecCommand(pod.Namespace, pod.Name, command)
		if e != nil {
			return e
		}
		fmt.Fprintf(os.Stderr, "%s\n", line)
	}
	return connect.Exec(ctx, config, pod.Namespace, pod.Name, command, os.Stdin, os.Stdout, os.Stderr, tty)
}
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if e := run(ctrl.SetupSignalHandler()); e != nil {
		slog.Error("process failed", "error", e)
		os.Exit(1)
	}
}
