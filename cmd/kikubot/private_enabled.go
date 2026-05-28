//go:build private

package main

// Pulls private tool implementations into the binary so their init()s run and
// they register themselves with internal/tools. Only compiled with -tags=private.
import _ "kikubot/internal/tools_priv"
