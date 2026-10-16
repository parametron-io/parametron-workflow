package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parametron-io/parametron-workflow/internal/config"
	"github.com/parametron-io/parametron-workflow/internal/github"
)

func commandFixture(t *testing.T) ([]string, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	source := config.SourceConfig{Organization: "org", Repositories: []string{"repo"}, Projects: map[config.Profile]config.ProjectBinding{}}
	for i, p := range []config.Profile{config.Engineering, config.BugTracker} {
		b := config.ProjectBinding{Number: i + 1, Fields: map[config.FieldRole]string{}, StatusOptions: map[config.StatusRole]string{}}
		roles := []config.FieldRole{config.Status, config.Priority, config.Effort, config.Estimate, config.StartDate}
		statuses := []config.StatusRole{config.Backlog, config.Ready, config.InProgress, config.InReview, config.Done}
		if p == config.Engineering {
			roles = append(roles, config.RoadmapOrder)
			statuses = append(statuses, config.Blocked)
		} else {
			roles = append(roles, config.PriorityScore)
			statuses = append(statuses, config.ToTriage)
		}
		for _, r := range roles {
			b.Fields[r] = string(r)
		}
		for _, s := range statuses {
			b.StatusOptions[s] = string(s)
		}
		source.Projects[p] = b
	}
	data, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	configPath, tokenPath, secretPath := filepath.Join(dir, "config.json"), filepath.Join(dir, "token"), filepath.Join(dir, "secret")
	for path, contents := range map[string][]byte{configPath: data, tokenPath: []byte("token\n"), secretPath: []byte("secret\n")} {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"--config", configPath, "--data-dir", filepath.Join(dir, "data"), "--github-token-file", tokenPath, "--webhook-secret-file", secretPath, "--github-credential-type", "user", "--feedback-author-id", "SYNTHETIC_USER", "--feedback-author-login", "test-controller", "--feedback-author-type", "User"}, configPath, tokenPath, secretPath
}
func TestLoad(t *testing.T) {
	args, _, tokenPath, _ := commandFixture(t)
	c, err := load(args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Engineering {
		t.Fatal("normal CLI must enable Engineering")
	}
	if c.Listen != "127.0.0.1:8080" || c.Concurrency != 1 || c.PollInterval.String() != "1s" || c.RetryDelay.String() != "30s" || string(c.WebhookSecret) != "secret" || c.Source.Organization != "org" {
		t.Fatal("defaults")
	}
	source := fileTokenSource{tokenPath}
	token, err := source.Token(context.Background())
	if err != nil || token != "token" {
		t.Fatal("credential adapter")
	}
	if err := os.WriteFile(tokenPath, []byte("rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	token, err = source.Token(context.Background())
	if err != nil || token != "rotated" {
		t.Fatal("credential not reread")
	}
	if err := os.WriteFile(tokenPath, []byte("sensitive\nembedded"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = source.Token(context.Background())
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("unsafe credential error")
	}
}

func TestNoEngineeringCLIFlag(t *testing.T) {
	args, _, _, _ := commandFixture(t)
	if _, err := load(append(args, "--engineering=false"), &bytes.Buffer{}); err == nil {
		t.Fatal("unexpected Engineering CLI flag")
	}
}
func TestLoadRejectsFiles(t *testing.T) {
	for _, name := range []string{"missing-config", "malformed-config", "missing-token", "empty-token", "newline-token", "missing-secret", "empty-secret", "newline-secret", "duration", "positional"} {
		t.Run(name, func(t *testing.T) {
			args, configPath, tokenPath, secretPath := commandFixture(t)
			var path string
			var contents []byte
			switch name {
			case "missing-config":
				os.Remove(configPath)
			case "malformed-config":
				path = configPath
				contents = []byte(`{"untrusted":"sensitive"}`)
			case "missing-token":
				os.Remove(tokenPath)
			case "empty-token":
				path = tokenPath
			case "newline-token":
				path = tokenPath
				contents = []byte("sensitive\nembedded")
			case "missing-secret":
				os.Remove(secretPath)
			case "empty-secret":
				path = secretPath
			case "newline-secret":
				path = secretPath
				contents = []byte("sensitive\nembedded")
			case "duration":
				args = append(args, "--retry-delay", "invalid")
			case "positional":
				args = append(args, "extra")
			}
			if path != "" {
				if err := os.WriteFile(path, contents, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := load(args, &bytes.Buffer{})
			if err == nil {
				t.Fatal("invalid files accepted")
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatal("content leaked")
			}
		})
	}
}

func TestSecretFileLineEndings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	for _, input := range []string{"token", "token\n", "token\r\n"} {
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if token, err := readSecret(path); err != nil || token != "token" {
			t.Fatal("valid line ending", err)
		}
	}
	for _, input := range []string{"token\r", "token\n\r\n", "token\r\n\n", "token\n\n", "token\x00", " token", "token "} {
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readSecret(path); err == nil {
			t.Fatal("malformed credential accepted")
		}
	}
}

func installationArgs(t *testing.T) []string {
	t.Helper()
	args, _, _, _ := commandFixture(t)
	var filtered []string
	for n := 0; n < len(args); n += 2 {
		switch args[n] {
		case "--github-token-file", "--github-credential-type", "--feedback-author-login", "--feedback-author-type":
			continue
		}
		filtered = append(filtered, args[n:n+2]...)
	}
	return append(filtered, "--github-credential-type", "installation", "--feedback-author-login", "synthetic-controller[bot]", "--feedback-author-type", "Bot", "--github-app-id", "101", "--github-app-client-id", "synthetic-client", "--github-app-slug", "synthetic-controller", "--github-installation-id", "202", "--github-installation-account-id", "303", "--github-installation-account-node-id", "SYNTHETIC_ORG", "--github-installation-account-type", "Organization", "--github-app-private-key-file", filepath.Join(t.TempDir(), "absent-key.pem"))
}
func TestLoadInstallationBindingsWithoutCredentialAccess(t *testing.T) {
	args := installationArgs(t)
	cfg, err := load(args, &bytes.Buffer{})
	if err != nil || cfg.Client == nil || cfg.FeedbackActor.Type != "Bot" {
		t.Fatal("structurally valid installation configuration rejected", err)
	}
	// Configuration is not authentication: the absent key must fail before any
	// network access when the shared writer identity is actually requested.
	if _, err := cfg.Client.(interface {
		AuthenticatedCommentAuthor(context.Context) (github.Actor, error)
	}).AuthenticatedCommentAuthor(context.Background()); err == nil {
		t.Fatal("missing key established trusted author")
	}
}
func TestLoadCredentialModesRejectMixedAndIncompleteBindings(t *testing.T) {
	for _, flag := range []string{"--github-app-id", "--github-app-client-id", "--github-app-slug", "--github-installation-id", "--github-installation-account-id", "--github-installation-account-node-id", "--github-installation-account-type", "--github-app-private-key-file"} {
		t.Run("user/"+flag, func(t *testing.T) {
			args, _, _, _ := commandFixture(t)
			value := "unexpected"
			if strings.HasSuffix(flag, "-id") && flag != "--github-app-client-id" && flag != "--github-installation-account-node-id" {
				value = "1"
			}
			if _, err := load(append(args, flag, value), &bytes.Buffer{}); err == nil {
				t.Fatal("mixed user/App configuration accepted")
			}
		})
	}
	for _, flag := range []string{"--github-app-id", "--github-app-client-id", "--github-app-slug", "--github-installation-id", "--github-installation-account-id", "--github-installation-account-node-id", "--github-installation-account-type", "--github-app-private-key-file", "--feedback-author-id", "--feedback-author-login", "--feedback-author-type"} {
		t.Run("installation/"+flag, func(t *testing.T) {
			args := installationArgs(t)
			var filtered []string
			for n := 0; n < len(args); n += 2 {
				if args[n] != flag {
					filtered = append(filtered, args[n:n+2]...)
				}
			}
			if _, err := load(filtered, &bytes.Buffer{}); err == nil {
				t.Fatal("missing explicit identity accepted")
			}
		})
	}
	args := installationArgs(t)
	if _, err := load(append(args, "--github-token-file", "opaque-token"), &bytes.Buffer{}); err == nil {
		t.Fatal("opaque installation token file accepted")
	}
}
