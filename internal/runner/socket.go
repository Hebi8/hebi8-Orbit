package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
)

// LocalDeliverRequest is sent by `orbit deliver` over ORBIT_RUNNER_SOCK.
type LocalDeliverRequest struct {
	AttemptID   string                   `json:"attemptId,omitempty"`
	TaskID      string                   `json:"taskId,omitempty"`
	CWD         string                   `json:"cwd,omitempty"`
	Summary     string                   `json:"summary"`
	Final       bool                     `json:"final"`
	Attachments []gitops.AttachmentInput `json:"attachments,omitempty"`
}

// LocalDeliverResponse is returned by the local runner socket to `orbit deliver`.
type LocalDeliverResponse struct {
	DeliveryID   string `json:"deliveryId"`
	AttemptID    string `json:"attemptId"`
	TaskID       string `json:"taskId"`
	ProjectID    string `json:"projectId"`
	BaseSHA      string `json:"baseSha"`
	HeadSHA      string `json:"headSha"`
	Finalized    bool   `json:"finalized"`
	ActiveSwarm  int    `json:"activeSwarm"`
	FilesChanged int    `json:"filesChanged"`
	Insertions   int    `json:"insertions"`
	Deletions    int    `json:"deletions"`
}

type localSocketServer struct {
	sockPath string
	listener net.Listener
	srv      *http.Server
}

func startLocalSocketServer(sockPath string, handler func(ctx context.Context, req LocalDeliverRequest) (LocalDeliverResponse, error)) (*localSocketServer, error) {
	if sockPath == "" {
		return nil, nil
	}
	absSock, err := filepath.Abs(sockPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absSock), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir socket dir: %w", err)
	}
	_ = os.Remove(absSock)

	ln, err := net.Listen("unix", absSock)
	if err != nil {
		return nil, fmt.Errorf("listen unix socket %s: %w", absSock, err)
	}
	_ = os.Chmod(absSock, 0o600)

	mux := http.NewServeMux()
	mux.HandleFunc("/deliver", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req LocalDeliverRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid json: " + err.Error()})
			return
		}
		resp, err := handler(r.Context(), req)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = srv.Serve(ln)
	}()

	return &localSocketServer{
		sockPath: absSock,
		listener: ln,
		srv:      srv,
	}, nil
}

func (s *localSocketServer) Close() error {
	if s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	_ = os.Remove(s.sockPath)
	return err
}
