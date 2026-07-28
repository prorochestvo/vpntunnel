// Package internal holds cross-cutting constant values shared across the
// vpntunnel binary, starting with the names of the environment variables the
// daemon reads at startup.
//
// It occupies the module's internal root, so every .go file placed directly
// under internal/ must declare package internal too; anything that wants its
// own package name belongs in a subdirectory.
package internal

// EnvTelegramBotDSN is the name of the environment variable carrying the
// optional Telegram bot DSN used for tunnel-change notifications. Only this
// NAME is safe to reference or log; the variable's VALUE embeds the bot token
// and must never appear in any log call, error message, or string format.
const EnvTelegramBotDSN = "VPNTUNNEL_TELEGRAMBOT_DSN"
