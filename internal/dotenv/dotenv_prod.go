//go:build !dev

/*
Env vars are loaded from docker in production.
*/

package dotenv

func LoadEnvFile() {
	// no-op in docker/prod builds
}
