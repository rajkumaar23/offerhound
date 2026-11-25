package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rajkumaar23/offerhound/internal/store"
)

// Gate pauses a run for a human (SMS/push/captcha). Implementations: CLI
// stdin prompt, web UI (via store), or remote relay.
type Gate interface {
	// Ask blocks until a human answers, the timeout fires, or ctx cancels.
	// Only the config-authored prompt string is relayed — never page
	// content, which could carry account PII.
	Ask(ctx context.Context, bank, prompt string, timeout time.Duration) (string, error)
}

// StoreGate blocks on a store-backed human request (local single-machine
// runs; remote runs use ServerGate in client.go).
type StoreGate struct {
	Store *store.Store
	User  string
}

func (g *StoreGate) Ask(ctx context.Context, bank, prompt string, timeout time.Duration) (string, error) {
	if g.Store == nil {
		return "", fmt.Errorf("gate: no store context")
	}
	id, err := g.Store.AskHuman(g.User, bank, prompt)
	if err != nil {
		return "", err
	}
	return g.Store.WaitForAnswer(ctx, id, timeout)
}

// StdinGate prompts on the terminal — headful single-machine UX.
type StdinGate struct{}

func (StdinGate) Ask(_ context.Context, _, prompt string, _ time.Duration) (string, error) {
	fmt.Printf("\n🐕 offerhound needs you: %s\n> ", prompt)
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return "", fmt.Errorf("no answer entered: %w", err)
	}
	return answer, nil
}

// resolveCredential materializes a credential reference at run time.
// Secrets never come from the YAML or the database.
func resolveCredential(ref string) (string, error) {
	i := strings.Index(ref, "://")
	if i <= 0 {
		return "", fmt.Errorf("malformed credential ref %q", ref)
	}
	scheme, rest := ref[:i], ref[i+3:]
	switch scheme {
	case "env":
		v := os.Getenv(rest)
		if v == "" {
			return "", fmt.Errorf("credential env var %s is not set", rest)
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(rest)
		if err != nil {
			return "", fmt.Errorf("credential file: %w", err)
		}
		return string(b), nil
	case "keyring":
		return "", fmt.Errorf("keyring backend not yet implemented; use env:// for now")
	default:
		return "", fmt.Errorf("unknown credential scheme %q", scheme)
	}
}
