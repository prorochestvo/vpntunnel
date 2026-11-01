package notify

import (
	"html"
	"strings"
)

// formatMessage renders the HTML notification body Telegram sends with
// parse_mode=HTML:
//
//	{tag} {title}
//	{details}                 (omitted when empty)
//	<pre>{filename}</pre>     (omitted when filename == "")
//
// tag is the caller-supplied, non-secret app identity (e.g. "#VPNTUNNEL"); the
// leading "{tag} " prefix is dropped when tag is empty. details is
// "{country} · exit {ip} ({city})" when exit is non-nil and has a non-empty IP
// (the "({city})" segment and the leading "{country} · " prefix are each
// dropped when empty); it falls back to just country when there is no exit
// info; the line is omitted entirely when neither is available. Every
// interpolated value is HTML-escaped; the caller-supplied tag prefix and the
// <pre> tags are not.
func formatMessage(tag, title, country string, exit *exitInfo, filename string) string {
	head := html.EscapeString(title)
	if tag != "" {
		head = tag + " " + head
	}
	lines := []string{head}

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
