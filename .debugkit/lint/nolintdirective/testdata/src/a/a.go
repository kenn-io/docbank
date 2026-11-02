package a

//nolint:kennlint // sleeptest: waits on a real process
var sleep int

//nolint:errcheck,kennlint // errtext: external stderr has no typed error
var multiple int

//nolint:errcheck // unrelated
var unrelated int

// This prose mentions //nolint:kennlint without declaring a directive.
var prose int

// want +2 "must name a kit rule and a reason"

//nolint:kennlint // sleeptest:
var missingReason int

// nolint:kennlint // a broad reason // want "must name a kit rule and a reason"
var spaced int

// nolint:KENNLINT // missing rule // want "must name a kit rule and a reason"
var uppercase int

//nolint:kennlint // sleeptest,deadlinetest: expiry is asserted against a real server
var combined int

//nolint:kennlint // sleeptest,unknown: a reason // want "must name a kit rule and a reason"
var unknownMember int

//nolint:kennlint // sleeptest,: a reason // want "must name a kit rule and a reason"
var emptyMember int
