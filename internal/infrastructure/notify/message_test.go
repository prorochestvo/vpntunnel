package notify

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatMessage(t *testing.T) {
	t.Parallel()

	t.Run("full message with title, country, exit, and filename", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", "switched tunnel", "se", &exitInfo{IP: "185.213.155.10", City: "Stockholm"}, "se-sto-wg-001.conf")

		want := "#VPNTUNNEL switched tunnel\n" +
			"se · exit 185.213.155.10 (Stockholm)\n" +
			"<pre>se-sto-wg-001.conf</pre>"
		assert.Equal(t, want, got)
	})

	t.Run("no exit info falls back to country only", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", "started", "se", nil, "se-sto-wg-001.conf")

		want := "#VPNTUNNEL started\nse\n<pre>se-sto-wg-001.conf</pre>"
		assert.Equal(t, want, got)
	})

	t.Run("no country and no exit omits the details line", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", "started", "", nil, "unknown.conf")

		want := "#VPNTUNNEL started\n<pre>unknown.conf</pre>"
		assert.Equal(t, want, got)
	})

	t.Run("exit without city omits the parenthetical", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", "started", "se", &exitInfo{IP: "185.213.155.10"}, "se-sto-wg-001.conf")

		assert.Contains(t, got, "se · exit 185.213.155.10\n")
		assert.NotContains(t, got, "(")
	})

	t.Run("exit without country drops the leading prefix", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", "started", "", &exitInfo{IP: "185.213.155.10", City: "Stockholm"}, "unknown.conf")

		assert.Contains(t, got, "exit 185.213.155.10 (Stockholm)\n")
		assert.NotContains(t, got, "·")
	})

	t.Run("empty filename omits the pre block", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", "started", "se", nil, "")

		assert.NotContains(t, got, "<pre>")
		assert.Equal(t, "#VPNTUNNEL started\nse", got)
	})

	t.Run("city with html special characters is escaped", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", "started", "se", &exitInfo{IP: "1.2.3.4", City: `<b>"evil"</b> & co`}, "x.conf")

		assert.NotContains(t, got, "<b>evil</b>")
		assert.Contains(t, got, "&lt;b&gt;&#34;evil&#34;&lt;/b&gt; &amp; co")
	})

	t.Run("filename with html special characters is escaped", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", "started", "se", nil, `<script>alert(1)</script>.conf`)

		assert.Contains(t, got, "<pre>&lt;script&gt;alert(1)&lt;/script&gt;.conf</pre>")
	})

	t.Run("crafted title is escaped", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", `<b>pwned</b>`, "", nil, "")

		assert.Equal(t, "#VPNTUNNEL &lt;b&gt;pwned&lt;/b&gt;", got)
	})

	t.Run("output contains the VPNTUNNEL prefix exactly once", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#VPNTUNNEL", "started", "se", &exitInfo{IP: "1.2.3.4", City: "X"}, "x.conf")

		assert.Equal(t, 1, strings.Count(got, "#VPNTUNNEL "))
	})

	t.Run("caller-supplied tag replaces the prefix", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("#OTHERAPP", "started", "se", nil, "")

		assert.Equal(t, "#OTHERAPP started\nse", got)
	})

	t.Run("empty tag omits the prefix", func(t *testing.T) {
		t.Parallel()
		got := formatMessage("", "started", "se", nil, "")

		assert.Equal(t, "started\nse", got)
	})
}
