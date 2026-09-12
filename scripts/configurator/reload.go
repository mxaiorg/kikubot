package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// knowledgeReloadTargets maps a knowledge scope to the docker-compose service
// keys that should reload. scope "common" (or empty) affects every agent (the
// shared knowledge dir); an agent email affects only that agent's service.
// Returns nil (no targets) when no service matches the scope.
func knowledgeReloadTargets(root, scope string) ([]string, error) {
	svcs, err := composeServices(root)
	if err != nil {
		return nil, err
	}
	if knowledgeDirName(scope) == knowledgeScopeCommon {
		keys := make([]string, 0, len(svcs))
		for _, s := range svcs {
			keys = append(keys, s.Key)
		}
		return keys, nil
	}
	stem := emailStem(scope)
	for _, s := range svcs {
		if s.Stem == stem {
			return []string{s.Key}, nil
		}
	}
	return nil, nil
}

// signalKnowledgeReload sends SIGHUP to the agent container(s) affected by a
// knowledge edit in `scope`, so the running agent reloads its knowledge base
// immediately instead of waiting for its ~30s poll.
//
// It is strictly best-effort: the edit is already persisted on disk and the
// poll-based reload is the correctness guarantee, so the caller should treat a
// returned error as an informational notice, not a save failure. When docker
// isn't on PATH at all (e.g. the configurator is run somewhere without it),
// reloaded is 0 and err is nil — we silently rely on the poll.
func signalKnowledgeReload(root, scope string) (reloaded int, err error) {
	targets, err := knowledgeReloadTargets(root, scope)
	if err != nil {
		return 0, err
	}
	return signalServices(root, targets)
}

// signalAgentsReload sends SIGHUP to the compose services of the given agent
// emails. The agent's SIGHUP handler reloads both its knowledge base and its
// tool set (agents.yaml tools: + mcp_servers.yaml), so this is the hook for
// tool-assignment and MCP catalog edits. Same best-effort contract as
// signalKnowledgeReload; emails without a compose service are skipped.
func signalAgentsReload(root string, emails []string) (reloaded int, err error) {
	svcs, err := composeServices(root)
	if err != nil {
		return 0, err
	}
	want := make(map[string]bool, len(emails))
	for _, e := range emails {
		want[emailStem(e)] = true
	}
	var targets []string
	for _, s := range svcs {
		if want[s.Stem] {
			targets = append(targets, s.Key)
		}
	}
	return signalServices(root, targets)
}

// signalServices runs `docker compose kill -s HUP <targets…>`. No docker on
// PATH or no targets → (0, nil): the agents' ~30s poll is the fallback.
func signalServices(root string, targets []string) (int, error) {
	if len(targets) == 0 {
		return 0, nil
	}
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		return 0, nil // no docker here; the poll will pick the edit up
	}
	if _, statErr := os.Stat(composePath(root)); statErr != nil {
		return 0, nil // nothing can be running from a compose file that doesn't exist
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := append([]string{"compose", "-f", composePath(root), "kill", "-s", "HUP"}, targets...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return 0, fmt.Errorf("%v: %s", runErr, strings.TrimSpace(string(out)))
	}
	return len(targets), nil
}

// reloadNote signals the given agents to hot-reload and returns a flash suffix
// describing the outcome for `what` (e.g. "tools", "MCP catalog"). Like
// knowledgeReloadNote, it always points at the poll-based fallback so a failed
// signal never reads as data loss.
func reloadNote(root string, emails []string, what string) string {
	n, err := signalAgentsReload(root, emails)
	switch {
	case err != nil:
		return " — agents reload " + what + " within ~30s (live-reload signal failed: " + err.Error() + ")"
	case n > 0:
		return fmt.Sprintf(" — signaled %d agent(s) to reload %s now", n, what)
	default:
		return " — agents reload " + what + " within ~30s"
	}
}

// knowledgeReloadNote signals the affected agent(s) to hot-reload and returns a
// short suffix to append to a success flash, describing the outcome. The note
// always reassures that the poll-based reload is the fallback, so a failed (or
// skipped) signal never reads as data loss.
func knowledgeReloadNote(root, scope string) string {
	n, err := signalKnowledgeReload(root, scope)
	switch {
	case err != nil:
		return " — agents reload within ~30s (live-reload signal failed: " + err.Error() + ")"
	case n > 0:
		return fmt.Sprintf(" — signaled %d agent(s) to reload now", n)
	default:
		return " — agents reload within ~30s"
	}
}
