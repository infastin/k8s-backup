package main

import (
	"errors"
	"strings"

	"github.com/infastin/gorack/validation"
	"github.com/infastin/gorack/validation/is/str"
	"github.com/infastin/gorack/xtypes"
)

type S3Config struct {
	EndpointURL     string          `env:"ENDPOINT_URL"`
	Region          string          `env:"REGION"`
	AccessKeyID     string          `env:"ACCESS_KEY_ID"`
	SecretAccessKey string          `env:"SECRET_ACCESS_KEY"`
	Bucket          string          `env:"BUCKET"`
	StorageClass    string          `env:"STORAGE_CLASS"`
	Unsecure        bool            `env:"UNSECURE"`
	ArchiveLifetime xtypes.Duration `env:"ARCHIVE_LIFETIME"`
}

func (c *S3Config) Validate() error {
	return validation.All(
		validation.String(c.EndpointURL, "endpoint_url").If(c.EndpointURL != "").With(isstr.URL).EndIf(),
		validation.String(c.AccessKeyID, "access_key_id").Required(true),
		validation.String(c.SecretAccessKey, "secret_access_key").Required(true),
		validation.String(c.Bucket, "bucket").Required(true),
		validation.Number(c.ArchiveLifetime, "archive_lifetime").GreaterEqual(0),
	)
}

type TelegramConfig struct {
	BotToken string `env:"BOT_TOKEN"`
	ChatID   int64  `env:"CHAT_ID"`
}

func (c *TelegramConfig) Validate() error {
	if c.BotToken == "" {
		return nil
	}
	return validation.All(
		validation.String(c.BotToken, "bot_token").Required(true),
		validation.Number(c.ChatID, "chat_id").Required(true),
	)
}

type ResourceConfig struct {
	ID        string `env:"ID"`
	Namespace string `env:"NAMESPACE"`
}

func (c *ResourceConfig) Validate() error {
	validID := func(s string) error {
		parts := strings.SplitN(s, "/", 2)
		if len(parts) == 1 {
			return errors.New("must be TYPE/NAME")
		}
		switch parts[0] {
		case "deployment", "deployments",
			"statefulset", "statefulsets",
			"replicaset", "replicasets":
		default:
			return errors.New("TYPE must be deployment(s), statefulset(s) or replicaset(s)")
		}
		if len(parts[1]) == 0 {
			return errors.New("NAME must not be empty")
		}
		return nil
	}
	return validation.All(
		validation.String(c.ID, "id").Required(true).With(validID),
		validation.String(c.Namespace, "namespace").Required(true),
	)
}

type BackupConfig struct {
	Directory string `env:"DIRECTORY"`
}

func (c *BackupConfig) Validate() error {
	return validation.All(
		validation.String(c.Directory, "directory").Required(true),
	)
}

type Config struct {
	Resource ResourceConfig `envPrefix:"RESOURCE_"`
	Backup   BackupConfig   `envPrefix:"BACKUP_"`
	S3       S3Config       `envPrefix:"S3_"`
	Telegram TelegramConfig `envPrefix:"TELEGRAM_"`
}

func (c *Config) Validate() error {
	return validation.All(
		validation.Ptr(&c.Resource, "resource").With(validation.Custom),
		validation.Ptr(&c.Backup, "backup").With(validation.Custom),
		validation.Ptr(&c.S3, "s3").With(validation.Custom),
		validation.Ptr(&c.Telegram, "telegram").With(validation.Custom),
	)
}
