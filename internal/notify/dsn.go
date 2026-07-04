package notify

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/prorochestvo/dsninjector"
)

// tokenPattern is the Bot API token shape: <bot-id>:<secret>.
var tokenPattern = regexp.MustCompile(`^\d{9,}:[a-zA-Z0-9_-]{35,}$`)

// tokenInURLPattern matches a bot token embedded in a Bot API request URL, as
// it appears inside the *url.Error the HTTP client returns on failure.
var tokenInURLPattern = regexp.MustCompile(`(/bot)\d{6,}:[a-zA-Z0-9_-]+`)

// parseDSN extracts the admin chat ID and bot token from a
// VPNTUNNEL_TELEGRAMBOT_DSN of the form tbot://<adminChatID>:@<botToken>/.
// The token is read from Addr() (the host:port pair), not Password(): a Bot
// API token is <bot-id>:<secret>, so dsninjector.Parse lands <bot-id> in host
// and <secret> in port, and Addr() rejoins them. Password() is empty in this
// form. The token shape is validated before use so a garbage DSN fails with a
// shape error rather than surfacing a partial token. Returned errors never
// contain the raw dsn or the token — dsninjector's own parse error can embed
// the input verbatim, so it is replaced with a generic message.
func parseDSN(dsn string) (adminChatID int64, token string, err error) {
	ds, perr := dsninjector.Parse(dsn)
	if perr != nil {
		return 0, "", errors.New("notify: malformed dsn")
	}

	token = strings.TrimSpace(ds.Addr())
	if !tokenPattern.MatchString(token) {
		return 0, "", errors.New("notify: bot token is required")
	}

	adminChatID, err = strconv.ParseInt(ds.Login(), 10, 64)
	if err != nil || adminChatID == 0 {
		return 0, "", errors.New("notify: admin chat id must be a non-zero integer")
	}

	return adminChatID, token, nil
}

// redactToken scrubs a bot token embedded in a URL-shaped error so the secret
// never reaches a log line. It returns a fresh error with the token replaced;
// nil in yields nil out.
func redactToken(err error) error {
	if err == nil {
		return nil
	}
	scrubbed := tokenInURLPattern.ReplaceAllString(err.Error(), "${1}<redacted>")
	return errors.New(scrubbed)
}
