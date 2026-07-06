package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// compile-time assertion that *TelegramNotifier satisfies Notifier.
var _ Notifier = (*TelegramNotifier)(nil)

// NewTelegram parses dsn (format tbot://<adminChatID>:@<botToken>/, where
// <botToken> is <bot-id>:<secret>), starts the notifier's asynchronous sender
// goroutine, and returns a ready TelegramNotifier. The caller must Close it
// on shutdown. The returned error is safe to log — it never contains the DSN
// or the bot token (see parseDSN). No identity probe (getMe) is performed at
// construction; a bad token only surfaces as a redacted warning on the first
// failed send.
func NewTelegram(dsn string, opLog *slog.Logger) (*TelegramNotifier, error) {
	adminChatID, token, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return newTelegramNotifier(adminChatID, token, "", "", nil, opLog), nil
}

// TelegramNotifier is a Notifier that posts tunnel-change events to a
// Telegram chat via the Bot API sendMessage endpoint. Notify is cheap and
// non-blocking: it applies the on-demand dedup/rate-limit check, then hands
// the event to a single background sender goroutine that performs the
// exit-IP probe, message formatting, and the HTTP send. Safe for concurrent
// use; Close is idempotent.
type TelegramNotifier struct {
	adminChatID int64
	token       string
	apiBase     string
	probeURL    string
	now         func() time.Time
	opLog       *slog.Logger

	ch     chan notifyJob
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	// mu guards the on-demand dedup/rate-limit state below. SourceStreaming
	// events never touch this state.
	mu                   sync.Mutex
	lastOnDemandFilename string
	lastOnDemandAt       time.Time
}

// Close stops the sender goroutine and waits for it to exit. It is safe to
// call more than once; only the first call has effect. Any event still
// queued when Close is called may or may not be sent — a cancelled internal
// context makes an in-flight probe/send fail fast, bounding the wait.
func (tn *TelegramNotifier) Close() {
	tn.once.Do(func() {
		tn.cancel()
	})
	<-tn.done
}

// Notify implements Notifier. For SourceOnDemand events it applies the
// dedup/rate-limit policy (suppressing repeats of the same tunnel within
// onDemandDedupWindow, and any switch within onDemandMinInterval of the last
// one) before enqueueing; SourceStreaming events bypass this entirely and are
// never suppressed. The enqueue is always non-blocking: a full queue drops
// the event with a warning rather than stalling the caller.
func (tn *TelegramNotifier) Notify(_ context.Context, ev Event) {
	if ev.Source == SourceOnDemand && tn.suppressed(ev.Filename) {
		return
	}

	select {
	case tn.ch <- notifyJob{ev: ev}:
	default:
		tn.opLog.Warn("notify: queue full, dropping event",
			slog.String("filename", ev.Filename),
		)
	}
}

// handle performs the actual notification work for one event: best-effort
// exit-IP probe, message formatting, and the Telegram send. It runs on the
// sender goroutine, using the notifier's own background ctx so a per-call
// caller ctx cancellation can never abort an in-flight send.
func (tn *TelegramNotifier) handle(ev Event) {
	var exit *exitInfo
	if ev.Dialer != nil {
		info, err := probeExitIP(tn.ctx, ev.Dialer, tn.probeURL)
		if err != nil {
			tn.opLog.Warn("notify: exit-ip probe failed", slog.String("err", err.Error()))
		} else {
			exit = &info
		}
	}

	text := formatMessage(ev.Title, ev.Country, exit, ev.Filename)
	if err := sendMessage(tn.ctx, tn.apiBase, tn.token, tn.adminChatID, text); err != nil {
		tn.opLog.Warn("notify: telegram send failed", slog.String("err", err.Error()))
	}
}

// sendLoop is the single sender goroutine. It exits when ctx is cancelled
// (Close called); any event already selected for processing when that
// happens still runs to completion, bounded by the now-cancelled ctx making
// its probe/send fail fast.
func (tn *TelegramNotifier) sendLoop() {
	defer close(tn.done)
	for {
		select {
		case <-tn.ctx.Done():
			return
		case job := <-tn.ch:
			tn.handle(job.ev)
		}
	}
}

// suppressed reports whether an on-demand event for filename should be
// dropped under the dedup/rate-limit policy, and — when it is not suppressed
// — records the state for the next check. State is updated on enqueue, not
// on send success, because the policy is best-effort and must not depend on
// the async send's outcome.
func (tn *TelegramNotifier) suppressed(filename string) bool {
	tn.mu.Lock()
	defer tn.mu.Unlock()

	now := tn.now()
	if filename == tn.lastOnDemandFilename && now.Sub(tn.lastOnDemandAt) < onDemandDedupWindow {
		return true
	}
	if now.Sub(tn.lastOnDemandAt) < onDemandMinInterval {
		return true
	}

	tn.lastOnDemandFilename = filename
	tn.lastOnDemandAt = now
	return false
}

// defaultAPIBase is the production Telegram Bot API base URL.
const defaultAPIBase = "https://api.telegram.org"

// sendTimeout bounds a single sendMessage HTTP round trip.
const sendTimeout = 10 * time.Second

// onDemandDedupWindow suppresses a repeated on-demand notification for the
// same tunnel filename within this window of the last one.
const onDemandDedupWindow = 5 * time.Minute

// onDemandMinInterval is the global floor between on-demand notifications
// regardless of which zone they report, protecting against rapid cross-zone
// switching flooding the chat.
const onDemandMinInterval = 20 * time.Second

// notifyQueueBuf is the capacity of the sender goroutine's job queue. Sized
// generously so a burst of notifications does not immediately drop events.
const notifyQueueBuf = 32

// sendClient is the package-level HTTP client used for the Bot API
// sendMessage call. Its Proxy is an explicit nil-returning func so requests
// never route through any process-wide proxy.
var sendClient = &http.Client{
	Timeout: sendTimeout,
	Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	},
}

// notifyJob is one unit of work handed from Notify to the sender goroutine.
type notifyJob struct {
	ev Event
}

// newTelegramNotifier builds a TelegramNotifier and starts its sender
// goroutine. apiBase and probeURL default to the production Telegram API and
// the production exit-IP probe endpoint (respectively) when empty; now
// defaults to time.Now; opLog defaults to slog.Default(). This is the shared
// construction path for both NewTelegram and tests, which inject apiBase/
// probeURL pointed at httptest servers and a fake now for the dedup window.
func newTelegramNotifier(adminChatID int64, token, apiBase, probeURL string, now func() time.Time, opLog *slog.Logger) *TelegramNotifier {
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	if now == nil {
		now = time.Now
	}
	if opLog == nil {
		opLog = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	tn := &TelegramNotifier{
		adminChatID: adminChatID,
		token:       token,
		apiBase:     apiBase,
		probeURL:    probeURL,
		now:         now,
		opLog:       opLog,
		ch:          make(chan notifyJob, notifyQueueBuf),
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	go tn.sendLoop()

	opLog.Info("telegram notifier enabled", slog.Int("token_len", len(token)))
	return tn
}

// sendMessage POSTs a form-encoded sendMessage request to the Telegram Bot
// API at {apiBase}/bot{token}/sendMessage. The bot token lives in the request
// path, so any *url.Error the client returns embeds it — every returned
// error is passed through redactToken before it reaches the caller.
func sendMessage(ctx context.Context, apiBase, token string, chatID int64, htmlText string) error {
	endpoint := apiBase + "/bot" + token + "/sendMessage"
	form := url.Values{
		"chat_id":                  {strconv.FormatInt(chatID, 10)},
		"text":                     {htmlText},
		"parse_mode":               {"HTML"},
		"disable_web_page_preview": {"true"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return redactToken(fmt.Errorf("notify: build sendMessage request: %w", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := sendClient.Do(req)
	if err != nil {
		return redactToken(fmt.Errorf("notify: sendMessage request failed: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		if readErr != nil {
			return redactToken(fmt.Errorf("notify: sendMessage status %d (body read failed: %w)", resp.StatusCode, readErr))
		}
		return redactToken(fmt.Errorf("notify: sendMessage status %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}

	return nil
}
