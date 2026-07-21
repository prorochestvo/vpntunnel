// Package constants holds cross-cutting constant values shared across the
// vpntunnel binary, starting with the names of the environment variables the
// daemon reads at startup.
package constants

// EnvTelegramBotDSN is the name of the environment variable carrying the
// optional Telegram bot DSN used for tunnel-change notifications. Only this
// NAME is safe to reference or log; the variable's VALUE embeds the bot token
// and must never appear in any log call, error message, or string format.
const EnvTelegramBotDSN = "VPNTUNNEL_TELEGRAMBOT_DSN"
