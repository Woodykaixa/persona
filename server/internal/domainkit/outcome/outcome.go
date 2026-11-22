// Package outcome is the one channel for expected failures: an Of is a value or a failure of the producer's sealed
// family, never both, and an Of nobody set is a defect wherever it is read, never a success. Producers name their
// result and set it on every path (`func Parse(raw string) (out outcome.Of[ID, Fail]) { ... return out.Ok(id) }`);
// callers read it through Fold, Unpack or the adapters' Widen and Then (docs/server/architecture.md "Domain model",
// "Failures").
//
//persona:layer kit
package outcome

import (
	"github.com/nwverse/persona/server/internal/domainkit/defect"
	"github.com/nwverse/persona/server/internal/domainkit/fail"
)

// set is which side of an Of holds: none (the zero Of), the value or the failure.
type set uint8

const (
	unset set = iota
	value
	failure
)

// Of is a value of type T or a failure of family F. The zero Of is unset: every reader raises a defect on it.
type Of[T any, F fail.Fail] struct {
	set     set
	value   T
	failure F
}

// Ok is the outcome holding v. Producers call it on their named result: return out.Ok(v).
func (o *Of[T, F]) Ok(v T) Of[T, F] { return Of[T, F]{set: value, value: v} }

// Err is the outcome holding f. Producers call it on their named result: return out.Err(f). A nil failure is a
// defect, and so is a zero one: Err visits f, and a base's Accept raises on its zero value.
func (o *Of[T, F]) Err(f F) Of[T, F] {
	if any(f) == nil {
		defect.Raise("outcome.Err", "a nil failure")
	}
	f.Accept(checked{})
	return Of[T, F]{set: failure, failure: f}
}

// checked is the visitor Err hands a failure to: visiting is the check, so it does nothing.
type checked struct{}

func (checked) VisitInvalid(fail.Code, fail.Part) {}
func (checked) VisitRefused(fail.Code)            {}
func (checked) VisitMissing(fail.Code)            {}
func (checked) VisitConflict(fail.Code)           {}

// IsOk says whether o holds a value.
func (o Of[T, F]) IsOk() bool { return o.held("outcome.IsOk") == value }

// held is which side o holds; an unset o is a defect.
func (o Of[T, F]) held(op string) set {
	o.expectSet(op)
	return o.set
}

// expectSet raises a defect, naming op, when o is unset: a reader that needs no side still never reads an unset o.
func (o Of[T, F]) expectSet(op string) {
	if o.set == unset {
		defect.Raise(op, "an unset outcome: its producer returned without Ok or Err")
	}
}

// Unpack is o's value and failure; exactly one of them is set (the failure is nil when o holds a value).
func Unpack[T any, F fail.Fail](o Of[T, F]) (T, F) {
	o.expectSet("outcome.Unpack")
	return o.value, o.failure
}

// Fold is onOk of o's value or onFail of its failure.
func Fold[T any, F fail.Fail, R any](o Of[T, F], onFail func(F) R, onOk func(T) R) R {
	if o.held("outcome.Fold") == failure {
		return onFail(o.failure)
	}
	return onOk(o.value)
}

// Widen is o with its failure as a fail.Fail, so an adapter can return failures of several families in one Of.
func Widen[T any, F fail.Fail](o Of[T, F]) Of[T, fail.Fail] {
	if o.held("outcome.Widen") == failure {
		return Of[T, fail.Fail]{set: failure, failure: o.failure}
	}
	return Of[T, fail.Fail]{set: value, value: o.value}
}

// Then is next of o's value, or o's failure: a step that may fail with another family, as an adapter chains a
// shared kernel's Parse and its own core's constructor.
func Then[A, B any, F, G fail.Fail](o Of[A, F], next func(A) Of[B, G]) Of[B, fail.Fail] {
	if o.held("outcome.Then") == failure {
		return Of[B, fail.Fail]{set: failure, failure: o.failure}
	}
	return Widen(next(o.value))
}

// Map is f of o's value, or o's failure.
func Map[A, B any, F fail.Fail](o Of[A, F], f func(A) B) Of[B, F] {
	if o.held("outcome.Map") == failure {
		return Of[B, F]{set: failure, failure: o.failure}
	}
	return Of[B, F]{set: value, value: f(o.value)}
}

// Must is o's value, for input the program made itself (a constant, an id it generated): a failure there is a
// programmer error, so it raises a defect naming op and the failure's kind.
func Must[T any, F fail.Fail](op string, o Of[T, F]) T {
	if o.held(op) == failure {
		o.failure.Accept(raising{op: op})
	}
	return o.value
}

// raising is the visitor Must hands a failure to: told the failure's kind, it raises the defect.
type raising struct{ op string }

func (r raising) VisitInvalid(fail.Code, fail.Part) { r.raise("invalid") }
func (r raising) VisitRefused(fail.Code)            { r.raise("refused") }
func (r raising) VisitMissing(fail.Code)            { r.raise("missing") }
func (r raising) VisitConflict(fail.Code)           { r.raise("conflict") }

func (r raising) raise(kind string) {
	defect.Raise(r.op, "a "+kind+" failure on input the program made")
}
