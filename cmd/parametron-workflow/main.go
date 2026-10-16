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
	tokenPath := fs.String("github-token-file", "", "user credential file (user mode only)")
	credentialType := fs.String("github-credential-type", "installation", "GitHub credential type: installation (App-owned minting) or user")
	appID := fs.Int64("github-app-id", 0, "expected GitHub App numeric ID (installation mode)")
	clientID := fs.String("github-app-client-id", "", "expected GitHub App client ID (installation mode)")
	slug := fs.String("github-app-slug", "", "expected GitHub App slug (installation mode)")
	installationID := fs.Int64("github-installation-id", 0, "expected GitHub App installation ID")
	accountID := fs.Int64("github-installation-account-id", 0, "expected installation account numeric ID")
	accountNodeID := fs.String("github-installation-account-node-id", "", "expected installation account node ID")
	accountType := fs.String("github-installation-account-type", "", "expected installation account type: Organization or User")
	keyPath := fs.String("github-app-private-key-file", "", "App RSA private key PEM file (installation mode only)")
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
	var tokens github.TokenSource
	if *credentialType == "user" {
		if *appID != 0 || *clientID != "" || *slug != "" || *installationID != 0 || *accountID != 0 || *accountNodeID != "" || *accountType != "" || *keyPath != "" {
			return app.Config{}, errors.New("App settings are not permitted in user credential mode")
		}
		if _, err = readSecret(*tokenPath); err != nil {
			return app.Config{}, err
		}
		tokens = typedTokenSource{fileTokenSource{*tokenPath}, "user"}
	} else {
		if *tokenPath != "" {
			return app.Config{}, errors.New("installation mode requires App-owned minting; token files are not accepted")
		}
		tokens, err = github.NewAppCredentials(github.AppCredentialConfig{
			AppID: *appID, ClientID: *clientID, Slug: *slug,
			InstallationID: *installationID, AccountID: *accountID,
			AccountLogin: source.Organization, AccountNodeID: *accountNodeID, AccountType: *accountType,
			PrivateKeyFile: *keyPath, Repositories: source.Repositories, Author: author,
		}, "", nil)
		if err != nil {
			return app.Config{}, errors.New("invalid explicit GitHub App credential bindings")
		}
	}
	secret, err := readSecret(*secretPath)
	if err != nil {
		return app.Config{}, err
	}
	client, err := github.NewTransport("", nil, tokens)
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
