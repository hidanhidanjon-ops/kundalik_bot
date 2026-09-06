package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	BotToken  string
	OwnerID   int64
	SecretKey string
}

func LoadConfig() *Config {
	_ = godotenv.Load()

	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		log.Fatal("BOT_TOKEN ko'rsatilmagan!")
	}

	ownerIDStr := os.Getenv("OWNER_ID")
	ownerID, err := strconv.ParseInt(ownerIDStr, 10, 64)
	if err != nil {
		log.Fatal("OWNER_ID noto'g'ri!")
	}

	secretKey := os.Getenv("SECRET_KEY")
	if secretKey == "" {
		secretKey = "default_secret_fallback_key_32b!"
	}

	return &Config{
		BotToken:  token,
		OwnerID:   ownerID,
		SecretKey: secretKey,
	}
}