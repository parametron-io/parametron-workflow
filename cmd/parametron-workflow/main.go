package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/parametron-io/parametron-workflow/internal/app"
	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "parametron-workflow: %v\n", err)
		os.Exit(1)
	}
}

// readSecret permits one conventional trailing LF/CRLF, but no embedded
// newline, NUL, or surrounding whitespace. Errors never include file contents.
func readSecret(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("secret file path required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file %q: %w", path, err)
	}
	value := string(data)
	if strings.HasSuffix(value, "\r\n") {
		value = strings.TrimSuffix(value, "\r\n")
	} else {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("invalid secret file %q", path)
	}
	return value, nil
}

// fileTokenSource is a replaceable deployment adapter, not authentication policy.
// It rereads only its configured file for each transport request.
type fileTokenSource struct{ path string }

type typedTokenSource struct {
	fileTokenSource
	kind string
}

func (s typedTokenSource) CommentCredentialKind() string { return s.kind }

func (s fileTokenSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	token, err := readSecret(s.path)
	if err != nil {
		return "", &github.Error{Category: github.Unauthorized}
	}
	return token, nil
}

func load(args []string, output io.Writer) (app.Config, error) {
	fs := flag.NewFlagSet("parametron-workflow", flag.ContinueOnError)
	fs.SetOutput(output)
	sourcePath := fs.String("config", "", "workflow bindings JSON file (required)")
	dataDir := fs.String("data-dir", "", "durable data directory (required)")
	listen := fs.String("listen", "127.0.0.1:8080", "HTTP listen address")
	tokenPath := fs.String("github-token-file", "", "GitHub credential file (required)")
	credentialType := fs.String("github-credential-type", "installation", "GitHub credential type: installation or user; unverifiable comment authors fail startup")
	secretPath := fs.String("webhook-secret-file", "", "webhook secret file (required)")
	authorID := fs.String("feedback-author-id", "", "trusted managed-comment author node ID (required)")
	authorLogin := fs.String("feedback-author-login", "", "canonical managed-comment author login (required)")
	authorType := fs.String("feedback-author-type", "", "managed-comment author account type: User or Bot (required)")
	concurrency := fs.Int("worker-concurrency", 1, "concurrent resource lanes")
	poll := fs.Duration("worker-poll-interval", time.Second, "worker poll interval (at least 1s)")
	retry := fs.Duration("retry-delay", 30*time.Second, "fixed positive retry delay")
	if err := fs.Parse(args); err != nil {
		return app.Config{}, err
	}
	if fs.NArg() != 0 {
		return app.Config{}, errors.New("unexpected positional arguments")
	}
	if *credentialType != "installation" && *credentialType != "user" {
		return app.Config{}, errors.New("invalid GitHub credential type")
	}
	author := github.Actor{ID: *authorID, Login: *authorLogin, Type: *authorType}
	if !github.ValidCommentAuthor(author) {
		return app.Config{}, errors.New("explicit valid managed-feedback author identity required")
	}
	f, err := os.Open(*sourcePath)
	if err != nil {
		return app.Config{}, fmt.Errorf("open configuration %q: %w", *sourcePath, err)
	}
	source, parseErr := config.Parse(f)
	closeErr := f.Close()
	// Parser diagnostics can contain user-authored content; keep startup bounded.
	if parseErr != nil {
		return app.Config{}, fmt.Errorf("invalid configuration %q", *sourcePath)
	}
	if closeErr != nil {
		return app.Config{}, closeErr
	}
	if _, err = readSecret(*tokenPath); err != nil {
		return app.Config{}, err
	}
	secret, err := readSecret(*secretPath)
	if err != nil {
		return app.Config{}, err
	}
	client, err := github.NewTransport("", nil, typedTokenSource{fileTokenSource{*tokenPath}, *credentialType})
	if err != nil {
		return app.Config{}, errors.New("GitHub transport construction failed")
	}
	return app.Config{Engineering: true, FeedbackActor: author, Source: source, Client: client, DataDir: *dataDir, Listen: *listen, WebhookSecret: []byte(secret), Concurrency: *concurrency, PollInterval: *poll, RetryDelay: *retry}, nil
}
func run() error {
	c, err := load(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, c)
}
