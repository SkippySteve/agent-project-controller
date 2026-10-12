package t3

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

//go:embed bridge.py
var bridge string

const Binary = "/opt/t3/t3"
const Data = "/workspace/.apc/t3"
const pidFile = Data + "/server.pid"

func Codex(args []string) error {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		cmd := exec.Command("/opt/codex/bin/codex", "--version")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	if len(args) == 0 || args[0] != "app-server" {
		return errors.New("T3 supports operator-managed Codex app-server sessions only")
	}
	return syscall.Exec("/usr/bin/python3", []string{"python3", "-c", bridge}, os.Environ())
}

func ensureWorker() error {
	if os.Getuid() != 1000 {
		return errors.New("non-root project worker required")
	}
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); !os.IsNotExist(err) {
		return errors.New("tokenless project worker required")
	}
	if _, err := os.Stat("/workspace/repo/.git"); err != nil {
		return errors.New("prepared project required")
	}
	if err := os.MkdirAll(Data+"/userdata", 0700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(Data)
	if err != nil || resolved != Data {
		return errors.New("T3 data directory must not use symlinks")
	}
	return nil
}

type processRecord struct {
	PID      int    `json:"pid"`
	Instance string `json:"instance"`
}

func activeRecord() (*processRecord, error) {
	raw, err := os.ReadFile(pidFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record processRecord
	if json.Unmarshal(raw, &record) != nil || record.PID <= 1 {
		return nil, errors.New("invalid T3 process identity")
	}
	target, err := os.Readlink("/proc/" + strconv.Itoa(record.PID) + "/exe")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if target != Binary {
		return nil, errors.New("T3 process identity changed")
	}
	return &record, nil
}

func Serve(instance string) error {
	if len(instance) != 32 {
		return errors.New("T3 launch instance required")
	}
	if err := ensureWorker(); err != nil {
		return err
	}
	if record, err := activeRecord(); err != nil {
		return err
	} else if record != nil {
		return errors.New("T3 is already running")
	}
	_ = os.Remove(pidFile)
	lock, err := os.OpenFile(pidFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("T3 launch already in progress")
	}
	rawRecord, _ := json.Marshal(processRecord{PID: os.Getpid(), Instance: instance})
	_, err = lock.Write(rawRecord)
	lock.Close()
	if err != nil {
		return err
	}
	settings := map[string]any{"providers": map[string]any{
		"codex": map[string]any{"setupMode": "existing", "enabled": true, "binaryPath": "/usr/local/bin/apc-codex", "homePath": "/workspace/.apc/codex"},
	}}
	for _, name := range []string{"claudeAgent", "cursor", "grok", "opencode", "antigravity"} {
		settings["providers"].(map[string]any)[name] = map[string]any{"enabled": false}
	}
	raw, _ := json.Marshal(settings)
	// Overwrite provider configuration on each launch. Device/session state is retained separately.
	if err := os.WriteFile(Data+"/userdata/settings.json", raw, 0600); err != nil {
		return err
	}
	return syscall.Exec(Binary, []string{Binary, "serve", "--host", "127.0.0.1", "--port", "8788", "--base-dir", Data, "--no-browser", "--auto-bootstrap-project-from-cwd", "/workspace/repo"}, os.Environ())
}

func Stop(instance string) error {
	if err := ensureWorker(); err != nil {
		return err
	}
	record, err := activeRecord()
	if err != nil {
		return err
	}
	if record == nil {
		return nil
	}
	if record.Instance != instance {
		return errors.New("T3 launch instance changed; refusing stop")
	}
	return syscall.Kill(record.PID, syscall.SIGTERM)
}
