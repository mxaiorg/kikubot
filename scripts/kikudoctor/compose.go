package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// composeFile is a minimal view of docker-compose.yml. yaml.v3 strips comments
// during Unmarshal, so this naturally analyses only uncommented lines.
type composeFile struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	EnvFile     yaml.Node `yaml:"env_file"`
	Environment yaml.Node `yaml:"environment"`
	Volumes     yaml.Node `yaml:"volumes"`
}

// parseCompose reads docker-compose.yml without writing to the report. Returns
// nil if missing or malformed; full validation runs later in validateCompose.
func parseCompose(root string) *composeFile {
	path := filepath.Join(root, "docker-compose.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cf composeFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil
	}
	if len(cf.Services) == 0 {
		return nil
	}
	return &cf
}

// localAgentAccounts derives the set of agent email accounts that this
// machine actually deploys, by inspecting each service's per-agent env_file
// (any *.env other than common.env). The stem of that file is the account.
func localAgentAccounts(cf *composeFile) map[string]bool {
	accounts := map[string]bool{}
	if cf == nil {
		return accounts
	}
	for _, svc := range cf.Services {
		for _, ef := range stringsFromScalarOrSeq(svc.EnvFile) {
			base := filepath.Base(ef)
			if !strings.HasSuffix(base, ".env") {
				continue
			}
			stem := strings.ToLower(strings.TrimSuffix(base, ".env"))
			if stem == "" || stem == "common" {
				continue
			}
			accounts[stem] = true
		}
	}
	return accounts
}

func validateCompose(root string, cf *composeFile, r *Report) {
	path := filepath.Join(root, "docker-compose.yml")
	sec := r.Section(fmt.Sprintf("docker-compose.yml (%s)", relPath(root, path)))

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			sec.Fail("file is missing")
		} else {
			sec.Fail("cannot read file: %v", err)
		}
		return
	}
	sec.Pass("file exists")

	if cf == nil {
		// parseCompose already failed silently; re-read to surface the reason.
		data, err := os.ReadFile(path)
		if err != nil {
			sec.Fail("cannot read file: %v", err)
			return
		}
		var probe composeFile
		if err := yaml.Unmarshal(data, &probe); err != nil {
			sec.Fail("file is not well-formed YAML: %v", err)
			return
		}
		sec.Fail("no services defined")
		return
	}
	sec.Pass("declares %d service(s)", len(cf.Services))

	composeDir := filepath.Dir(path)
	names := make([]string, 0, len(cf.Services))
	for n := range cf.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := cf.Services[name]
		ssec := r.Section(fmt.Sprintf("  service: %s", name))

		envFiles := stringsFromScalarOrSeq(svc.EnvFile)
		if len(envFiles) == 0 {
			ssec.Warn("no env_file declared")
		} else {
			missing := 0
			for _, ef := range envFiles {
				p := ef
				if !filepath.IsAbs(p) {
					p = filepath.Join(composeDir, p)
				}
				if _, err := os.Stat(p); err != nil {
					if os.IsNotExist(err) {
						ssec.Fail("env_file %q does not exist", ef)
					} else {
						ssec.Fail("env_file %q cannot be stat'd: %v", ef, err)
					}
					missing++
				}
			}
			if missing == 0 {
				ssec.Pass("all %d env_file(s) exist", len(envFiles))
			}
		}

		env := environmentMap(svc.Environment)
		if v, ok := env["RUNNING_IN_CONTAINER"]; !ok {
			ssec.Fail("RUNNING_IN_CONTAINER is not set")
		} else if !strings.EqualFold(strings.TrimSpace(v), "true") {
			ssec.Fail("RUNNING_IN_CONTAINER is %q, expected \"true\"", v)
		} else {
			ssec.Pass("RUNNING_IN_CONTAINER=true")
		}

		if hasDataVolume(svc.Volumes) {
			ssec.Pass("volume mount preserves /app/data")
		} else {
			ssec.Fail("no volume mount targets /app/data (agent data not preserved)")
		}
	}
}

// stringsFromScalarOrSeq accepts either a single scalar or a sequence of
// scalars (compose accepts both shapes for env_file).
func stringsFromScalarOrSeq(n yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value == "" {
			return nil
		}
		return []string{n.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				out = append(out, c.Value)
			}
		}
		return out
	}
	return nil
}

// environmentMap reads a compose `environment` block, which may be either a
// map of KEY: VALUE or a sequence of "KEY=VALUE" / "KEY" entries.
func environmentMap(n yaml.Node) map[string]string {
	out := map[string]string{}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			if k.Kind == yaml.ScalarNode && v.Kind == yaml.ScalarNode {
				out[k.Value] = v.Value
			}
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				continue
			}
			if eq := strings.Index(c.Value, "="); eq >= 0 {
				out[c.Value[:eq]] = c.Value[eq+1:]
			} else {
				out[c.Value] = os.Getenv(c.Value)
			}
		}
	}
	return out
}

// hasDataVolume returns true if any volume mount targets /app/data. Volumes
// can be short-form ("./data/x:/app/data") or long-form ({type, source, target}).
func hasDataVolume(n yaml.Node) bool {
	if n.Kind != yaml.SequenceNode {
		return false
	}
	for _, c := range n.Content {
		switch c.Kind {
		case yaml.ScalarNode:
			parts := strings.Split(c.Value, ":")
			if len(parts) >= 2 && strings.HasPrefix(parts[1], "/app/data") {
				return true
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(c.Content); i += 2 {
				k := c.Content[i]
				v := c.Content[i+1]
				if k.Kind == yaml.ScalarNode && k.Value == "target" && v.Kind == yaml.ScalarNode {
					if strings.HasPrefix(v.Value, "/app/data") {
						return true
					}
				}
			}
		}
	}
	return false
}
