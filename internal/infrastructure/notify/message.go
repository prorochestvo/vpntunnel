package notify

import (
	"html"
	"strings"
)

// formatMessage renders the HTML notification body Telegram sends with
// parse_mode=HTML:
//
//	#VPNTUNNEL {title}
//	{details}                 (omitted when empty)
//	<pre>{filename}</pre>     (omitted when filename == "")
//
// details is "{country} · exit {ip} ({city})" when exit is non-nil and has a
// non-empty IP (the "({city})" segment and the leading "{country} · " prefix
// are each dropped when empty); it falls back to just country when there is
// no exit info; the line is omitted entirely when neither is available. Every
// interpolated value is HTML-escaped; the literal "#VPNTUNNEL" prefix and the
// <pre> tags are not.
func formatMessage(title, country string, exit *exitInfo, filename string) string {
	lines := []string{"#VPNTUNNEL " + html.EscapeString(title)}

	if details := formatDetails(country, exit); details != "" {
		lines = append(lines, details)
	}
	if filename != "" {
		lines = append(lines, "<pre>"+html.EscapeString(filename)+"</pre>")
	}

	return strings.Join(lines, "\n")
}

// formatDetails builds the optional details line for formatMessage.
func formatDetails(country string, exit *exitInfo) string {
	escapedCountry := html.EscapeString(country)

	if exit == nil || exit.IP == "" {
		return escapedCountry
	}

	detail := "exit " + html.EscapeString(exit.IP)
	if exit.City != "" {
		detail += " (" + html.EscapeString(exit.City) + ")"
	}
	if escapedCountry != "" {
		return escapedCountry + " · " + detail
	}
	return detail
}
