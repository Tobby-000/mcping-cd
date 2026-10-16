package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type deployRequest struct {
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
	Tag   string `json:"tag"`
	Image string `json:"image"`
}

type config struct {
	deployToken     string
	allowedRegistry string
	composeDir      string
	composeFile     string
	listenAddr      string
}

func loadConfig() (*config, error) {
	c := &config{
		deployToken:     os.Getenv("DEPLOY_TOKEN"),
		allowedRegistry: os.Getenv("ALLOWED_REGISTRY"),
		composeDir:      os.Getenv("COMPOSE_DIR"),
		composeFile:     os.Getenv("COMPOSE_FILE"),
		listenAddr:      os.Getenv("LISTEN_ADDR"),
	}
	if c.listenAddr == "" {
		c.listenAddr = ":9000"
	}
	switch {
	case c.deployToken == "":
		return nil, errors.New("DEPLOY_TOKEN is required")
	case c.allowedRegistry == "":
		return nil, errors.New("ALLOWED_REGISTRY is required")
	case c.composeDir == "":
		return nil, errors.New("COMPOSE_DIR is required")
	}
	return c, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           routes(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      6 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func routes(cfg *config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/deploy", deployHandler(cfg))
	return mux
}

var deployMu sync.Mutex

func deployHandler(cfg *config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

		token := r.Header.Get("X-Deploy-Token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.deployToken)) != 1 {
			log.Printf("unauthorized request from %s", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if !deployMu.TryLock() {
			http.Error(w, "deploy in progress", http.StatusConflict)
			return
		}
		defer deployMu.Unlock()

		var req deployRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		if !strings.HasPrefix(req.Image, cfg.allowedRegistry) {
			log.Printf("rejected image %q: not in allowed registry", req.Image)
			http.Error(w, "image not allowed", http.StatusBadRequest)
			return
		}

		fullImage := req.Image
		if req.Tag != "" {
			fullImage = req.Image + ":" + req.Tag
		}

		log.Printf("deploy start: %s (repo=%s sha=%s)", fullImage, req.Repo, req.SHA)

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()

		if out, err := run(ctx, "", "docker", "pull", fullImage); err != nil {
			log.Printf("docker pull failed: %v\n%s", err, out)
			http.Error(w, "pull failed", http.StatusInternalServerError)
			return
		}

		args := []string{"compose"}
		if cfg.composeFile != "" {
			args = append(args, "-f", cfg.composeFile)
		}
		args = append(args, "up", "-d")
		if out, err := run(ctx, cfg.composeDir, "docker", args...); err != nil {
			log.Printf("docker compose up failed: %v\n%s", err, out)
			http.Error(w, "deploy failed", http.StatusInternalServerError)
			return
		}

		log.Printf("deploy success: %s", fullImage)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "deployed",
			"image":  fullImage,
		})
	}
}

func run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}