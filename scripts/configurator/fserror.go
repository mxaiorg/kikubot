package main

import (
	"errors"
	"fmt"
	"io/fs"
)

// fsWriteError annotates a filesystem error from a save/write/delete with the
// offending path and, for the common permission-denied case, an explicit
// remedy. The configurator often runs as a different user than the one that
// owns the config tree it edits (configs/knowledge, configs/agents.yaml,
// configs/secrets.env, the DMS config dir, …) — frequently a bind-mounted
// volume — so "permission denied" is the failure operators hit most. The raw
// os error ("open …: permission denied") doesn't tell them what to do about
// it; the returned message is surfaced verbatim in the UI flash. The newline
// is rendered (flash text is pre-wrap), so the remedy lands on its own line.
func fsWriteError(path string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("permission denied writing %s\nThe configurator can't write this path. Make sure the file and its parent directory are writable by the user running the configurator — check the ownership and permissions (chmod/chown) of %s and the directory it lives in — then try again.", path, path)
	}
	return fmt.Errorf("could not write %s: %w", path, err)
}
