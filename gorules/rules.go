//go:build ruleguard

// Package gorules holds the review rules that no upstream linter implements.
// They are interpreted by gocritic's ruleguard check, so this file is never
// compiled into a binary and needs no entry in go.mod.
//
// A rule belongs here only when it is expressible as a local AST pattern.
// Rules that need flow analysis across statements (for example "this rows
// iteration is never followed by rows.Err()") cannot be written in this DSL
// and stay manual-review items.
package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// errorLoggedAndReturned catches double reporting: an error that is both
// written to the log and propagated is counted twice by whoever reads the log.
// The error itself must be the returned value. Logging and then degrading to a
// fallback is a legitimate single handling and must not be reported.
func errorLoggedAndReturned(m dsl.Matcher) {
	m.Match(
		`if $err != nil { $log.Printf($*_); return $err }`,
		`if $err != nil { $log.Println($*_); return $err }`,
		`if $err != nil { $log.Print($*_); return $err }`,
		`if $err != nil { $log.Error($*_); return $err }`,
		`if $err != nil { $log.Errorf($*_); return $err }`,
		`if $err != nil { $log.Printf($*_); return $*_, $err }`,
		`if $err != nil { $log.Println($*_); return $*_, $err }`,
		`if $err != nil { $log.Print($*_); return $*_, $err }`,
		`if $err != nil { $log.Error($*_); return $*_, $err }`,
		`if $err != nil { $log.Errorf($*_); return $*_, $err }`,
	).
		Where(m["err"].Type.Is(`error`)).
		Report(`error is logged and returned: handle it once — log at the boundary or propagate, never both`)
}

// httpErrorWithoutReturn catches a handler that writes an error response and
// then keeps running, producing a second write to the same ResponseWriter.
func httpErrorWithoutReturn(m dsl.Matcher) {
	m.Match(`if $*_ { $*_; http.Error($w, $*_) }`).
		Where(m["w"].Type.Implements(`net/http.ResponseWriter`)).
		Report(`http.Error does not stop the handler: add a return, or the response is written twice`)
}

// stringConcatInLoop catches quadratic string building.
func stringConcatInLoop(m dsl.Matcher) {
	m.Match(
		`for { $*_; $s += $_; $*_ }`,
		`for $*_; $*_; $*_ { $*_; $s += $_; $*_ }`,
		`for $_, $_ := range $_ { $*_; $s += $_; $*_ }`,
		`for $_ := range $_ { $*_; $s += $_; $*_ }`,
	).
		Where(m["s"].Type.Is(`string`)).
		Report(`string concatenation in a loop is quadratic: use strings.Builder, with Grow when the size is known`)
}

// httpClientWithoutTimeout catches the client that waits forever.
func httpClientWithoutTimeout(m dsl.Matcher) {
	m.Match(
		`http.Client{}`,
		`http.Client{Transport: $_}`,
		`http.Client{Transport: $_, $*_}`,
	).
		Where(!m.File().Name.Matches(`_test\.go$`)).
		Report(`http.Client without Timeout blocks forever on a stalled peer: set Timeout explicitly`)
}

// timeComparedWithOperator catches == on time.Time, which also compares the
// monotonic reading and the location — a parsed timestamp never matches a
// time.Now() value even when they denote the same instant.
func timeComparedWithOperator(m dsl.Matcher) {
	m.Match(`$t0 == $t1`, `$t0 != $t1`).
		Where(m["t0"].Type.Is(`time.Time`) && m["t1"].Type.Is(`time.Time`)).
		Report(`compare time.Time with Equal: == also compares the monotonic clock and the location`)
}

// floatMoney catches currency held in binary floating point. The name list is
// deliberately narrow: "cost", "sum", "total" and "fee" also name token
// counters and provider telemetry, where a float is the honest type.
func floatMoney(m dsl.Matcher) {
	m.Match(`var $name $t = $_`, `var $name $t`).
		Where(m["t"].Type.Is(`float64`) &&
			m["name"].Text.Matches(`(?i)(amount|price|balance|payment|money|invoice|refund)`)).
		Report(`money in float64 loses fractions: store integer minor units or use a decimal type`)
}

// sleepInProductionCode catches a sleep used as a synchronisation primitive.
func sleepInProductionCode(m dsl.Matcher) {
	m.Match(`time.Sleep($_)`).
		Where(!m.File().Name.Matches(`_test\.go$`)).
		Report(`time.Sleep as synchronisation: wait on a channel, a context, or a timer instead`)
}

// Deliberately absent, and kept absent on purpose:
//
//   - unbounded errgroup fan-out — a local pattern cannot tell whether
//     SetLimit is called later, so the rule would fire on correct code.
//   - detached context.Background() deep in a call chain — the contextcheck
//     linter already decides this with real flow analysis.
//   - a nil check next to len() — staticcheck S1009 already reports it.
//   - a rows.Next() loop with no rows.Err() afterwards — needs flow analysis
//     across statements, which this DSL cannot express.
//
// The first two are review noise; the third is a genuine gap and stays a
// manual-review item until a go/analysis analyzer covers it.
