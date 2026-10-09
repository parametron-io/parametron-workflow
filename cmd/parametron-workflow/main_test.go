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
	return []string{"--config", configPath, "--data-dir", filepath.Join(dir, "data"), "--github-token-file", tokenPath, "--webhook-secret-file", secretPath}, configPath, tokenPath, secretPath
}
func TestLoad(t *testing.T) {
	args, _, tokenPath, _ := commandFixture(t)
	c, err := load(args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
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
