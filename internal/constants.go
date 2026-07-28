// Package internal holds the cross-cutting application-policy values shared
// across the vpntunnel binary: the names of the environment variables the
// daemon reads at startup, the defaults applied to absent config keys, and the
// SSRF deny list. They live here rather than in the packages that consume them
// because deciding these values is a policy call, not part of parsing a config
// file or filtering an IP. Each concern keeps its own file.
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
