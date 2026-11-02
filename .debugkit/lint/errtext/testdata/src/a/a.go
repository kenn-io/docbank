package a

import (
	"errors"
	"strings"
)

var ErrNotFound = errors.New("not found")

type codedError struct{ code int }

func (e *codedError) Error() string { return "coded" }

func classify(err error) int {
	switch {
	case strings.Contains(err.Error(), "required"): // want "matching on err.Error\\(\\) text"
		return 400
	case strings.HasPrefix(err.Error(), "not found"): // want "matching on err.Error\\(\\) text"
		return 404
	case err.Error() == "conflict": // want "matching on err.Error\\(\\) text"
		return 409
	case "gone" != err.Error(): // want "matching on err.Error\\(\\) text"
		return 410
	case strings.EqualFold("Teapot", (err.Error())): // want "matching on err.Error\\(\\) text"
		return 418
	}
	return 500
}

func classifyTyped(err *codedError) bool {
	return strings.Contains(err.Error(), "coded") // want "matching on err.Error\\(\\) text"
}

func fine(err error) (int, string) {
	if errors.Is(err, ErrNotFound) {
		return 404, ""
	}
	if coded, ok := errors.AsType[*codedError](err); ok {
		return coded.code, ""
	}
	msg := err.Error()
	if strings.Contains("static text", "text") {
		return 0, msg
	}
	return 0, "prefix: " + err.Error()
}

type notAnError struct{}

func (notAnError) Error() (string, bool) { return "", false }

func unrelated(n notAnError) bool {
	text, _ := n.Error()
	return strings.Contains(text, "x")
}
