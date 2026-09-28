package main

import (
	"os"
	"strings"
	"testing"
)

func TestWarnIfLooksLikeProd(t *testing.T) {
	cases := []struct {
		name string
		addr string
		warn bool
	}{
		{"in-cluster minikube DNS", "agent-service.telara-agents.svc.cluster.local:50051", false},
		{"localhost port-forward", "localhost:50051", false},
		{"127.0.0.1 port-forward", "127.0.0.1:50051", false},
		{"explicit prod hostname", "agent-service.prod.telara.internal:50051", true},
		{"public telara.dev domain", "agent-service.telara.dev:443", true},
		{"public domain but in-cluster suffix (contradictory, still fine to flag)", "prod.svc.cluster.local:50051", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, w, _ := os.Pipe()
			old := os.Stderr
			os.Stderr = w
			defer func() { os.Stderr = old }()

			warnIfLooksLikeProd(c.addr)

			w.Close()
			buf := make([]byte, 4096)
			n, _ := r.Read(buf)
			out := string(buf[:n])

			if c.warn && !strings.Contains(out, "warning:") {
				t.Errorf("expected a warning for addr %q, got none", c.addr)
			}
			if !c.warn && out != "" {
				t.Errorf("expected no warning for addr %q, got: %q", c.addr, out)
			}
		})
	}
}
