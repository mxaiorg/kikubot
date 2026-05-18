//go:build dev

package dotenv

import (
	"errors"
	"io/fs"
	"log"

	"github.com/joho/godotenv"
)

// LoadEnvFile loads ./configs/secrets.env into the process environment when
// the binary is built with -tags=dev. The file is gitignored and holds
// API keys + per-agent EMAIL_PASSWORD entries. Missing-file is non-fatal —
// the operator may set the same variables via shell export or an IDE
// run-configuration instead.
func LoadEnvFile() {
	err := godotenv.Overload("./configs/secrets.env")
	if err == nil {
		log.Println("[DEBUG] loaded ./configs/secrets.env")
		return
	}
	if errors.Is(err, fs.ErrNotExist) {
		log.Println("[DEBUG] ./configs/secrets.env not found — using shell environment only")
		return
	}
	log.Printf("[DEBUG] error loading ./configs/secrets.env: %v (continuing with shell environment)", err)
}
