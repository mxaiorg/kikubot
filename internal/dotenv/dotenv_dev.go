//go:build dev

package dotenv

import (
	"log"

	"github.com/joho/godotenv"
)

func LoadEnvFile() {
	err := godotenv.Overload("./configs/env/common.env")
	if err != nil {
		panic(err)
	}
	log.Println("[DEBUG] USING COMMON ENVIRONMENT VARIABLES")
	err2 := godotenv.Overload("./configs/env/kiku.env")
	if err2 == nil {
		log.Println("[DEBUG] USING DEV ENVIRONMENT VARIABLES")
	}
}
