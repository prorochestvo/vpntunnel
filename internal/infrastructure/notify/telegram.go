package notify

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/prorochestvo/dsninjector"
)

// NewTelegram builds a Telegram notifier from a DataSource (parsed by the
// caller from a DSN of the form tbot://<adminChatID>:@<botToken>/, where
// <botToken> is <bot-id>:<secret>). tag is the non-secret app identity prefixed
// to every message (e.g. "#VPNTUNNEL"), supplied by the caller so a different
// app reusing this package can brand its own notifications. NewTelegram starts
// the notifier's asynchronous sender goroutine and returns a ready
// TelegramNotifier; the caller must Close it on shutdown. The returned error is
// safe to log — it never contains the DSN or the bot token (see extractIdentity
// and the bot-init path). No identity probe (getMe) is performed at construction
// (WithSkipGetMe); a bad token only surfaces as a redacted warning on the first
// failed send.
func NewTelegram(ds dsninjector.DataSource, tag string, opLog *slog.Logger) (*TelegramNotifier, error) {
	adminChatID, token, err := extractIdentity(ds)
	if err != nil {
		return nil, err
	}
	return newTelegramNotifier(adminChatID, token, tag, "", "", nil, opLog)
}

// TelegramNotifier is a Notifier that posts tunnel-change events to a Telegram
// chat via the go-telegram/bot SendMessage call. Notify is cheap and
// non-blocking: it applies the on-demand dedup/rate-limit check, then hands the
// event to a single background sender goroutine that performs the exit-IP probe,
// message formatting, and the send. Safe for concurrent use; Close is idempotent.
type TelegramNotifier struct {
	adminChatID int64
	tag         string
	bot         *bot.Bot
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

	text := formatMessage(tn.tag, ev.Title, ev.Country, exit, ev.Filename)
	_, err := tn.bot.SendMessage(tn.ctx, &bot.SendMessageParams{
		ChatID:             tn.adminChatID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: boolPtr(true)},
	})
	if err != nil {
		// go-telegram/bot redacts the bot token from its errors (it replaces the
		// token in any *url.Error URL, and API errors carry only Telegram's
		// description), so err is safe to log.
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

// sendTimeout bounds a single SendMessage HTTP round trip.
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

// sendClient is the HTTP client handed to go-telegram/bot for the Bot API
// call. Its Proxy is an explicit nil-returning func so requests never route
// through any process-wide proxy.
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
// goroutine. apiBase overrides the Telegram API base URL (empty = the library
// default, https://api.telegram.org); now defaults to time.Now; opLog defaults
// to slog.Default(). This is the shared construction path for both NewTelegram
// and tests, which inject apiBase pointed at an httptest server and a fake now
// for the dedup window. The returned error is safe to log (never the token).
func newTelegramNotifier(adminChatID int64, token, tag, apiBase, probeURL string, now func() time.Time, opLog *slog.Logger) (*TelegramNotifier, error) {
	if now == nil {
		now = time.Now
	}
	if opLog == nil {
		opLog = slog.Default()
	}

	opts := []bot.Option{
		bot.WithSkipGetMe(),
		bot.WithHTTPClient(sendTimeout, sendClient),
	}
	if apiBase != "" {
		opts = append(opts, bot.WithServerURL(apiBase))
	}
	b, err := bot.New(token, opts...)
	if err != nil {
		// bot.New's error can reference the token; never surface it.
		return nil, errors.New("notify: telegram bot init failed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	tn := &TelegramNotifier{
		adminChatID: adminChatID,
		tag:         tag,
		bot:         b,
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
	return tn, nil
}

// boolPtr returns a pointer to b, for the *bool fields in the bot API models.
func boolPtr(b bool) *bool { return &b }
