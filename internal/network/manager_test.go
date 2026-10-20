package network

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/clooooud/wsl-tunneling/internal/config"
	"github.com/clooooud/wsl-tunneling/internal/wsl"
)

type scriptClient struct {
	wsl.Client // Other methods are unused by startTunnel.
	scripts    []string
	result     wsl.Result
	failCall   int
	err        error
}

func (client *scriptClient) Bash(_ context.Context, _ string, script string) (wsl.Result, error) {
	client.scripts = append(client.scripts, script)
	if len(client.scripts) == client.failCall {
		return wsl.Result{}, client.err
	}
	if len(client.scripts) == 1 {
		return client.result, nil
	}
	return wsl.Result{}, nil
}

func TestStartTunnelInteropHandoff(t *testing.T) {
	sentinel := errors.New("script failure")
	for _, tt := range []struct {
		name     string
		stdout   string
		failCall int
		calls    int
	}{
		{"new helper", "forwarder_started=1\n", 0, 2},
		{"new helper with profile output", "shell profile\nforwarder_started=1\n", 0, 2},
		{"healthy existing", "", 0, 1},
		{"unrelated stdout", "not_forwarder_started=1\n", 0, 1},
		{"start failure", "", 1, 1},
		{"post-handoff failure", "forwarder_started=1\n", 2, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptClient{result: wsl.Result{Stdout: tt.stdout}, failCall: tt.failCall, err: sentinel}
			manager := Manager{Config: config.Defaults(), WSL: client}
			err := manager.startTunnel(context.Background(), "/proxy", "/forwarder", []string{"corp.example.test"}, nil)
			if (tt.failCall != 0 && !errors.Is(err, sentinel)) || (tt.failCall == 0 && err != nil) {
				t.Fatalf("startTunnel error = %v, failCall = %d", err, tt.failCall)
			}
			if len(client.scripts) != tt.calls {
				t.Fatalf("Bash calls = %d, want %d", len(client.scripts), tt.calls)
			}
			for _, script := range client.scripts {
				if !strings.Contains(script, "DNS_SEARCH='corp.example.test'") {
					t.Fatal("DNS suffixes must be retained through handoff")
				}
			}
			if tt.calls == 2 && !strings.Contains(client.scripts[1], "wait_for_network 15 6") {
				t.Fatal("new helper must be validated for consecutive stability after handoff")
			}
		})
	}
}
