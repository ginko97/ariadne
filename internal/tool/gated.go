package tool

// DefaultGated are the built-in tools that ask before every call unless the
// operator trusts them by name.
//
// Here rather than in cmd/ariadne because two other places have to agree with
// it: the approval preview decides what a card shows per tool, and the eval
// task sets have to approve a gated tool explicitly or the gate answers for
// the model being measured. A second copy of this list in a test is a copy
// that goes stale.
//
// Each one reaches past the conversation. web_fetch is the outbound channel:
// the model chooses the URL, so one GET can carry out anything it has read.
// edit_file changes a file in place. write_file replaces a file whole, which
// is the one that damages a folder fastest — the model must reproduce every
// line it did not mean to change, and whatever it drops is gone.
//
// exec is not in the list because nothing may take it out of the gate; the
// agent forces it separately.
var DefaultGated = []string{WebFetchName, EditFileName, WriteFileName}

// Gated reports whether name is gated by default.
func Gated(name string) bool {
	for _, n := range DefaultGated {
		if n == name {
			return true
		}
	}
	return false
}
