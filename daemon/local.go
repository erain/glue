package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultLocalShutdownTimeout = 5 * time.Second

// Metadata is the local connection file shape written by local daemon
// launchers and consumed by daemon clients.
type Metadata struct {
	Version int    `json:"version"`
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
	PID     int    `json:"pid"`
}

// LocalConfig configures ServeLocal.
type LocalConfig struct {
	Name            string
	ListenAddr      string
	Token           string
	TokenSource     string
	MetadataPath    string
	ShutdownTimeout time.Duration
}

// ServeLocal starts handler on a local TCP listener, writes optional
// connection metadata, and shuts down gracefully when ctx is canceled.
func ServeLocal(ctx context.Context, cfg LocalConfig, handler http.Handler, stdout io.Writer) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultLocalShutdownTimeout
	}
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = "glue daemon"
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	baseURL := "http://" + ln.Addr().String()
	if cfg.MetadataPath != "" {
		if err := WriteMetadata(cfg.MetadataPath, Metadata{
			Version: 1,
			BaseURL: baseURL,
			Token:   cfg.Token,
			PID:     os.Getpid(),
		}); err != nil {
			_ = ln.Close()
			return err
		}
	}

	if stdout != nil {
		fmt.Fprintf(stdout, "%s listening\n", name)
		fmt.Fprintf(stdout, "base_url: %s\n", baseURL)
		if cfg.MetadataPath != "" {
			fmt.Fprintf(stdout, "metadata: %s\n", cfg.MetadataPath)
			fmt.Fprintf(stdout, "token: written to metadata file (%s)\n", cfg.TokenSource)
		} else {
			fmt.Fprintf(stdout, "token: configured (%s); metadata file disabled\n", cfg.TokenSource)
		}
	}

	server := &http.Server{Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		err := server.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return <-errCh
	}
}

// ResolveToken returns an explicit token, $GLUE_DAEMON_TOKEN, or a new
// random token, along with a human-readable source string.
func ResolveToken(flagValue string) (token, source string, err error) {
	if token := strings.TrimSpace(flagValue); token != "" {
		return token, "flag", nil
	}
	if token := strings.TrimSpace(os.Getenv("GLUE_DAEMON_TOKEN")); token != "" {
		return token, "GLUE_DAEMON_TOKEN", nil
	}
	token, err = RandomToken()
	if err != nil {
		return "", "", err
	}
	return token, "generated", nil
}

// RandomToken returns a 256-bit random bearer token encoded as hex.
func RandomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// DefaultMetadataPath returns the shared local daemon metadata path.
func DefaultMetadataPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return filepath.Join(".glue", "daemon.json")
	}
	return filepath.Join(dir, "glue", "daemon.json")
}

// ReadMetadata reads and validates local daemon connection metadata.
func ReadMetadata(path string) (Metadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Metadata{}, err
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return Metadata{}, err
	}
	if meta.Version != 1 {
		return Metadata{}, fmt.Errorf("metadata %s: unsupported version %d", path, meta.Version)
	}
	return meta, nil
}

// WriteMetadata writes local daemon connection metadata with owner-only
// permissions.
func WriteMetadata(path string, meta Metadata) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}
